//go:build integration

// Package integration is the coding-agent → waired provider → gateway →
// local-model ROUTING SENTINEL (#496). It rides an already-running enrolled
// daemon (fed via WAIRED_* env vars — the install test's Tier-2 harness stands
// it up) and, for each coding-agent "leg", drives a real inference request at
// the exact gateway surface that tool's config points at, then proves via the
// daemon's observability event ring that the completion was SERVED LOCALLY and
// did NOT fail open to real Anthropic.
//
// Why drive the gateway surface (a curl) rather than the real tool binary: it
// is deterministic and reproduces the dominant "inference errors" class
// (gateway routing / proxy fail-open / model-not-ready) precisely, without a
// per-tool binary + auth handshake on three OSes. The real-tool end-to-end
// (`claude -p`, `opencode run`, OpenClaw) against the real bundled model is the
// separate, heavier #518. The config-write half (does the plugin / managed
// settings surface the provider) is exercised here via each tool's real
// integration writer, plus the wiring unit tests.
//
// Extensibility (#496 priority): a new leg is one Leg literal in legs() — the
// runner, sentinel, and CI wiring are untouched.
//
// Run with a live enrolled daemon:
//
//	WAIRED_MGMT_URL=http://127.0.0.1:9476 WAIRED_TINY_ALIAS=waired/tiny \
//	  go test -tags integration ./internal/e2e/integration/...
//
// Skips cleanly when the daemon is unreachable (the enrolled daemon is the
// missing prerequisite, same stance as the ollama/codeui integration tests).
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/management/ipcclient"
	"github.com/waired-ai/waired-agent/internal/management/observabilityclient"
	"github.com/waired-ai/waired-agent/internal/observability"
)

// Env is the world the harness reasons over, populated from WAIRED_* env vars
// by the per-OS install-test hook (scripts/dev/lib/installtest-integration.sh).
type Env struct {
	// MgmtURL is the loopback management API base (observability event ring +
	// inference status). Default http://127.0.0.1:9476.
	MgmtURL string
	// ClaudeURL is the Claude managed-settings loopback proxy base (the
	// intercept :9472). Default http://127.0.0.1:9472.
	ClaudeURL string
	// DataPlaneURL is the no-token OpenCode/OpenClaw data-plane gateway base
	// (:9479). Default http://127.0.0.1:9479.
	DataPlaneURL string
	// TinyAlias is the catalog alias/id the legs request. Default waired/tiny.
	TinyAlias string
	// Only, when non-empty, restricts the run to a comma-separated leg name set
	// (WAIRED_INTEGRATION_LEGS), e.g. "claude,opencode".
	Only map[string]bool
}

func env(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// LoadEnv reads the WAIRED_* contract, applying loopback defaults.
func LoadEnv() Env {
	e := Env{
		MgmtURL:      strings.TrimRight(env("WAIRED_MGMT_URL", "http://127.0.0.1:9476"), "/"),
		ClaudeURL:    strings.TrimRight(env("WAIRED_CLAUDE_GATEWAY_URL", "http://127.0.0.1:9472"), "/"),
		DataPlaneURL: strings.TrimRight(env("WAIRED_OPENCODE_GATEWAY_URL", "http://127.0.0.1:9479"), "/"),
		TinyAlias:    env("WAIRED_TINY_ALIAS", "waired/tiny"),
	}
	if only := strings.TrimSpace(os.Getenv("WAIRED_INTEGRATION_LEGS")); only != "" {
		e.Only = map[string]bool{}
		for _, n := range strings.Split(only, ",") {
			if n = strings.TrimSpace(n); n != "" {
				e.Only[n] = true
			}
		}
	}
	return e
}

// Leg is the per-tool contract. Adding a coding agent = appending one Leg to
// legs(); the runner and sentinel are untouched.
type Leg struct {
	// Name is the leg identifier (also the WAIRED_INTEGRATION_LEGS filter key).
	Name string
	// ExpectKind is the observability RequestEvent.Kind the drive produces:
	// "anthropic" (Claude /v1/messages) or "openai" (/v1/chat/completions).
	ExpectKind string
	// Configure writes the tool's real provider config (proving the config
	// surface) and returns a teardown. nil configure is allowed (Claude needs
	// none — the intercept proxy is the surface).
	Configure func(ctx context.Context, e Env) (func(), error)
	// Drive issues ONE inference request at the gateway surface the tool's
	// config targets, returning the HTTP status + body for diagnostics.
	Drive func(ctx context.Context, e Env) (status int, body []byte, err error)
}

// --- HTTP drives ---

var driveClient = &http.Client{Timeout: 2 * time.Minute}

// driveAnthropic POSTs an Anthropic-shaped request at baseURL/v1/messages with
// a deliberately-bogus auth token: a regression that fails open to real
// Anthropic gets a 401 (and records no local event), so the sentinel catches it.
func driveAnthropic(ctx context.Context, baseURL, model string) (int, []byte, error) {
	body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"Reply with one word: hi"}]}`, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "waired-integration-dummy-not-a-real-key")
	return do(req)
}

// driveOpenAI POSTs an OpenAI-compatible chat request at
// baseURL/v1/chat/completions — the exact wire request the OpenCode / OpenClaw
// waired provider plugins make against the no-token data-plane gateway.
func driveOpenAI(ctx context.Context, baseURL, model string) (int, []byte, error) {
	body := fmt.Sprintf(`{"model":%q,"stream":false,"messages":[{"role":"user","content":"Reply with one word: hi"}]}`, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(req)
}

func do(req *http.Request) (int, []byte, error) {
	resp, err := driveClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// --- sentinel ---

// ringCursor snapshots the current event-ring high-water mark so the post-drive
// query only sees events this leg produced.
func ringCursor(ctx context.Context, e Env) (uint64, error) {
	resp, err := observabilityclient.GetEvents(ctx, e.MgmtURL, 0, []observability.Kind{observability.KindRequest}, 0)
	if err != nil {
		return 0, err
	}
	return resp.NextSince, nil
}

// awaitLocalRequest polls the event ring from `since` until a KindRequest event
// of the wanted kind, served locally (decision=="local") with a 2xx status,
// appears — the proof the completion was served by the local gateway and did
// NOT fail open (the intercept passthrough bypasses the recorder, so a
// fail-open produces no such event). Returns the matching event, or the best
// (non-local / error) event seen for diagnostics.
func awaitLocalRequest(ctx context.Context, e Env, since uint64, wantKind string, timeout time.Duration) (*observability.RequestEvent, error) {
	deadline := time.Now().Add(timeout)
	var last *observability.RequestEvent
	for {
		resp, err := observabilityclient.GetEvents(ctx, e.MgmtURL, since, []observability.Kind{observability.KindRequest}, 0)
		if err == nil {
			for i := range resp.Events {
				r := resp.Events[i].Request
				if r == nil || r.Kind != wantKind {
					continue
				}
				last = r
				if r.Decision == "local" && r.Status >= 200 && r.Status < 300 {
					return r, nil
				}
			}
		}
		if time.Now().After(deadline) {
			if last != nil {
				return nil, fmt.Errorf("no local 2xx %s request event; last was decision=%q status=%d error_reason=%q",
					wantKind, last.Decision, last.Status, last.ErrorReason)
			}
			return nil, fmt.Errorf("no %s request event recorded within %s (fail-open, or the request never reached the gateway)", wantKind, timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// daemonReachable reports whether the management API answers (the "enrolled
// daemon is the missing prerequisite → skip" gate).
func daemonReachable(e Env) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := ringCursor(ctx, e)
	return err == nil
}

// pullTinyModel asks the daemon to pull + ready the tiny routing model, so the
// harness is self-sufficient when the shell hook hasn't pre-pulled it. Idempotent.
func pullTinyModel(ctx context.Context, e Env) error {
	body, _ := json.Marshal(map[string]string{"model": e.TinyAlias})
	// models/pull is a mutating verb: since waired#838 it travels over the
	// local IPC socket, and the loopback TCP port (e.MgmtURL, still used for
	// the reads above) refuses it with 403.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ipcclient.BaseURL+"/waired/v1/models/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := ipcclient.NewHTTPClient(60 * time.Second).Do(req)
	if err != nil {
		return ipcclient.WrapDialError(err)
	}
	defer httpResp.Body.Close()
	resp, _ := io.ReadAll(httpResp.Body)
	if httpResp.StatusCode >= 400 {
		return fmt.Errorf("models/pull %s: HTTP %d: %s", e.TinyAlias, httpResp.StatusCode, resp)
	}
	return nil
}

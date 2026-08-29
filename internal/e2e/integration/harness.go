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
// What each leg asserts is the ROUTING DECISION the daemon recorded, not
// what the transport did. The two came apart in waired-agent#1091: naming a
// model the real Anthropic API serves pins the turn upstream, and on the
// Linux lane — which blackholes api.anthropic.com — that turn cannot reach
// the upstream, degrades back to local (#665), and is recorded decision=local
// exactly like a turn that never left. So a leg declares an Outcome and the
// run checks it against the daemon's own record of where the id sent it,
// which reads the same on every OS and with or without the blackhole
// (waired-agent#1141).
//
// Extensibility (#496 priority): a new leg is one Leg literal in legs() — the
// runner, sentinel, and CI wiring are untouched.
//
// Run with a live enrolled daemon:
//
//	WAIRED_MGMT_URL=http://127.0.0.1:9476 WAIRED_TINY_ALIAS=waired/tiny \
//	  go test -tags integration ./internal/e2e/integration/...
//
// Skips when the daemon is unreachable AND the caller did not name one.
// Naming one (WAIRED_MGMT_URL, which all three installtest harnesses set)
// is asking for the sentinel to run, and "the daemon is not there" is a
// failure of that request rather than an absence of work — the same
// distinction #956 settled for the agent-harness lane. A bare
// `go test -tags integration ./...` names nothing and still skips, which
// is the case the skip was written for (waired-agent#1118).
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

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/ipcclient"
	"github.com/waired-ai/waired-agent/internal/management/observabilityclient"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
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
	// Default http://127.0.0.1:9473.
	DataPlaneURL string
	// TinyAlias is the catalog alias/id the legs request. Default waired/tiny.
	TinyAlias string
	// Only, when non-empty, restricts the run to a comma-separated leg name set
	// (WAIRED_INTEGRATION_LEGS), e.g. "claude,opencode". A name that matches
	// no leg is an error, not an empty selection: see unknownLegs.
	Only map[string]bool
	// MgmtNamed reports whether the caller set WAIRED_MGMT_URL rather than
	// falling back to the loopback default. It separates "run the sentinel
	// against this daemon" from "run whatever happens to be there".
	MgmtNamed bool
	// SummaryPath, when set, is where the run writes what it actually did.
	SummaryPath string
	// AnthropicBlackholed reports whether the run points api.anthropic.com at
	// 0.0.0.0 (the CI fail-open guard, WAIRED_ANTHROPIC_BLACKHOLED=1). With the
	// guard armed, "cannot reach the upstream API" is not a network blip: it
	// proves the request already escaped local routing, so the sentinel's core
	// assertion has failed and retrying is pointless.
	AnthropicBlackholed bool
}

// The environment contract, named once. Read in more than one place —
// LoadEnv, and the guards that have to know whether the CALLER asked for
// something — and two spellings of the same variable is how a guard comes
// to be looking at a name nobody sets.
const (
	mgmtURLEnv = "WAIRED_MGMT_URL"
	legsEnv    = "WAIRED_INTEGRATION_LEGS"
	// summaryEnv names a file the run writes one leg name per line to, in
	// the order they were served locally. It is the wrapper's evidence:
	// the shell used to assert "every leg served locally" from the exit
	// status of `go test`, which is satisfied by this package's untagged
	// arithmetic tests alone.
	summaryEnv = "WAIRED_INTEGRATION_SUMMARY"
)

func env(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// LoadEnv reads the WAIRED_* contract, applying loopback defaults.
func LoadEnv() Env {
	e := Env{
		MgmtURL:      strings.TrimRight(env(mgmtURLEnv, "http://127.0.0.1:9476"), "/"),
		ClaudeURL:    strings.TrimRight(env("WAIRED_CLAUDE_GATEWAY_URL", "http://127.0.0.1:9472"), "/"),
		DataPlaneURL: strings.TrimRight(env("WAIRED_LOCAL_GATEWAY_URL", "http://127.0.0.1:9473"), "/"),
		TinyAlias:    env("WAIRED_TINY_ALIAS", "waired/tiny"),
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WAIRED_ANTHROPIC_BLACKHOLED"))) {
	case "1", "true", "yes":
		e.AnthropicBlackholed = true
	}
	e.MgmtNamed = strings.TrimSpace(os.Getenv(mgmtURLEnv)) != ""
	e.SummaryPath = strings.TrimSpace(os.Getenv(summaryEnv))
	if only := strings.TrimSpace(os.Getenv(legsEnv)); only != "" {
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
	// config targets, returning the response for diagnostics.
	Drive func(ctx context.Context, e Env) (driveResponse, error)
	// Expect is what this leg claims about its turn: served on this device,
	// or routed to the real Anthropic API.
	//
	// It is checked against the daemon's OWN record of the routing decision,
	// not against what the transport happened to do. A lane that blackholes
	// the upstream drags an anthropic-routed turn back to local (#665), so
	// on the wire the two are the same local 2xx — which is how #1091
	// inverted this table's meaning and passed the per-PR sentinel green
	// (waired-agent#1141).
	Expect Outcome
	// Model is the id this leg drives, resolved against Env. Compared with
	// the daemon's record of the last requested id so a record left by the
	// PREVIOUS leg cannot satisfy this one's assertion.
	Model string
	// SubagentClass marks a leg whose turn the daemon classifies as
	// subagent traffic. RecordRequest deliberately drops that class
	// (cmd/waired-agent/claude_routing.go: recording it would overwrite the
	// main conversation's model with a string the user never chose), so the
	// routing record must NOT be read for such a leg — it would still hold
	// the previous leg's turn. The fail-open header check still applies.
	SubagentClass bool
}

// routeRecorded reports whether the daemon's Claude routing state will hold
// THIS leg's turn. Only the Claude intercept surface feeds it — the OpenAI
// data-plane legs drive the gateway directly (:9473) and never pass through
// the intercept at all — and subagent-class traffic is excluded there by
// design. Asked as a question about the leg so that a leg which cannot be
// checked this way says so, rather than silently reading a stale record.
func routeRecorded(leg Leg) bool {
	return leg.ExpectKind == "anthropic" && !leg.SubagentClass
}

// --- HTTP drives ---

// driveResponse is one drive attempt's response. It carries the HEADERS the
// old (status, body) pair threw away: on a Claude-leg fail-open the local
// error is discarded before the upstream replay, and X-Waired-Fallback is the
// only evidence of it left on the wire. Dropping that header is exactly why a
// dead model runner read as "waired proxy could not reach the upstream API"
// for a week (waired-agent#29).
type driveResponse struct {
	Status int
	Header http.Header
	Body   []byte
}

// driveClient bounds a request as a backstop only; the real per-attempt bound
// is the context in the drive loop (driveAttemptTimeout), which is what keeps
// one slow attempt from overrunning the whole leg budget.
var driveClient = &http.Client{Timeout: driveAttemptTimeout + 15*time.Second}

// driveAnthropic POSTs an Anthropic-shaped request at baseURL/v1/messages with
// a deliberately-bogus auth token: a regression that fails open to real
// Anthropic gets a 401 (and records no local event), so the sentinel catches it.
func driveAnthropic(ctx context.Context, baseURL, model string) (driveResponse, error) {
	body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"Reply with one word: hi"}]}`, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		return driveResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-api-key", "waired-integration-dummy-not-a-real-key")
	return do(req)
}

// driveOpenAI POSTs an OpenAI-compatible chat request at
// baseURL/v1/chat/completions — the exact wire request the OpenCode / OpenClaw
// waired provider plugins make against the no-token data-plane gateway.
func driveOpenAI(ctx context.Context, baseURL, model string) (driveResponse, error) {
	body := fmt.Sprintf(`{"model":%q,"stream":false,"messages":[{"role":"user","content":"Reply with one word: hi"}]}`, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return driveResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	return do(req)
}

func do(req *http.Request) (driveResponse, error) {
	resp, err := driveClient.Do(req)
	if err != nil {
		return driveResponse{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return driveResponse{Status: resp.StatusCode, Header: resp.Header, Body: b}, nil
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

// ringSummary renders the LAST KindRequest event of wantKind since the cursor,
// whatever its decision/status, or "" if there is none.
//
// The drive-failure path needs it because on the Claude legs the real local
// status survives ONLY here: the gateway records the engine's status
// (rr.fail(500, "upstream_error")) while the intercept discards the staged
// error body before replaying upstream, so the client sees an opaque 502.
func ringSummary(ctx context.Context, e Env, since uint64, wantKind string) string {
	resp, err := observabilityclient.GetEvents(ctx, e.MgmtURL, since, []observability.Kind{observability.KindRequest}, 0)
	if err != nil {
		return fmt.Sprintf("(event ring unreadable: %v)", err)
	}
	var last *observability.RequestEvent
	for i := range resp.Events {
		if r := resp.Events[i].Request; r != nil && r.Kind == wantKind {
			last = r
		}
	}
	if last == nil {
		return ""
	}
	return fmt.Sprintf("kind=%s decision=%s status=%d error_reason=%q model=%s latency=%dms ttft=%dms",
		last.Kind, last.Decision, last.Status, last.ErrorReason, last.Model, last.LatencyMs, last.TTFTMs)
}

// driveFailureDetail assembles everything known about a failed drive into one
// block: the wire response, the local status recovered from the fail-open
// header, the event-ring record, and where the engine's own reason lives.
// waired-agent#29 took a week to diagnose because each of these existed but
// none of them was ever printed together with the others.
func driveFailureDetail(e Env, since uint64, wantKind string, last driveResponse) string {
	// A fresh context on purpose: the caller's leg context is frequently the
	// thing that just expired, and the ring read is the most valuable line in
	// the block precisely on that path.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var b strings.Builder
	fmt.Fprintf(&b, "        last response: HTTP %d\n", last.Status)
	if fb := last.Header.Get(headerFallback); fb != "" {
		fmt.Fprintf(&b, "        %s: %s\n", headerFallback, fb)
		if local, named, ok := localStatusFromFallback(fb); ok {
			switch {
			case local != 0:
				fmt.Fprintf(&b, "        local status:  %d (discarded by internal/proxy/intercept before the upstream replay)\n", local)
			default:
				fmt.Fprintf(&b, "        local error:   %s (discarded before the upstream replay)\n", named)
			}
		}
	}
	if s := ringSummary(ctx, e, since, wantKind); s != "" {
		fmt.Fprintf(&b, "        event ring:    %s\n", s)
	}
	// The routing record answers the question the ring cannot: a turn that
	// went upstream is served by nothing here, so it leaves no ring event at
	// all — and "no event" reads the same as "the request never arrived".
	// waired-agent#29's lesson is that each of these existed and none was
	// ever printed beside the others.
	if wantKind == "anthropic" {
		fmt.Fprintf(&b, "        routing record: %s\n", routeSummary(ctx))
	}
	fmt.Fprintf(&b, "        body:          %s\n", truncate(last.Body))
	// The path, not an artifact name. This used to name routing-sentinel-diag,
	// which only routing-sentinel.yml produces: the nightly's own legs upload
	// under other names, and its Windows leg uploaded nothing at all — so on
	// 2026-08-28 this line was printed on a red Windows leg pointing at an
	// artifact that did not exist (waired-agent#1112). engine.log.1 is named
	// because the engine rotates on every spawn, so a crash-loop leaves the
	// informative attempt there.
	b.WriteString("        next:          the engine's own reason is NOT on the wire. See engine.log\n")
	b.WriteString("                       and engine.log.1 under <state-dir>/runtimes/<engine>/logs/ —\n")
	b.WriteString("                       a CI job that collects them uploads them as its diagnostics\n")
	b.WriteString("                       artifact.\n")
	return b.String()
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

// --- the routing decision, as the daemon recorded it ---

// routeAnthropic is the recorded route meaning "this turn went to the real
// Anthropic API". Taken from the daemon's own vocabulary rather than
// re-typed, so a rename cannot leave this harness comparing against a
// string nobody writes any more.
var routeAnthropic = string(state.ClaudeRouteAnthropic)

// claudeRoutingState reads the daemon's record of the last main-class Claude
// turn: the model id it carried, and the route that id resolved to.
//
// This is the sentinel's ONLY transport-independent view of the routing
// decision, and #1091 added it for exactly the reason the sentinel needs it:
// "a turn the user sent to the real Anthropic API by naming a model never
// reaches RecordServed", so asking the host what the last turn ASKED FOR is
// what makes a session routed somewhere unexpected visible at all
// (waired-agent#1036). The record is written at the top of dispatchRoute,
// before the upstream is attempted, so it still says "anthropic" on a lane
// where the blackhole forced the #665 degrade back to local — which is the
// whole reason it can be asserted on identically across the three OS legs.
//
// Socket-only: the route is not in management's tcpReadRoutes, so like
// models/pull it travels over the local IPC socket / named pipe.
func claudeRoutingState(ctx context.Context) (management.ClaudeRoutingState, error) {
	var st management.ClaudeRoutingState
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ipcclient.BaseURL+"/waired/v1/integration/claude/route", nil)
	if err != nil {
		return st, err
	}
	resp, err := ipcclient.NewHTTPClient(10 * time.Second).Do(req)
	if err != nil {
		return st, ipcclient.WrapDialError(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return st, fmt.Errorf("GET claude/route: HTTP %d: %s", resp.StatusCode, truncate(body))
	}
	if err := json.Unmarshal(body, &st); err != nil {
		return st, fmt.Errorf("decode claude/route: %w (body %s)", err, truncate(body))
	}
	return st, nil
}

// routeSummary renders the recorded decision for a failure block.
func routeSummary(ctx context.Context) string {
	st, err := claudeRoutingState(ctx)
	if err != nil {
		return fmt.Sprintf("(routing record unreadable: %v)", err)
	}
	if st.LastRequestModel == "" {
		return "(no Claude turn recorded this daemon lifetime)"
	}
	return fmt.Sprintf("model=%s route=%s at=%s",
		st.LastRequestModel, st.LastRequestRoute, st.LastRequestAt.Format(time.RFC3339))
}

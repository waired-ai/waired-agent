package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/router"
)

// OpenAIError mirrors the OpenAI error envelope so clients with strict
// error-shape parsers (e.g. older SDKs) don't blow up.
type OpenAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}

type openAIErrorEnvelope struct {
	Error OpenAIError `json:"error"`
}

// handleOpenAIModels returns the catalog mapped to OpenAI's
// `{data:[{id,object,owned_by,...}]}` shape. The dynamic coding
// aliases (waired/default, waired/coding — resolved by the router to
// the host's current default, #632), every manifest's model_id, and
// its static aliases are all listed so client SDKs that pre-validate
// the model field accept any spelling.
func (h *HandlerSet) handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "GET only")
		return
	}
	manifests := h.deps.ListManifests()
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	created := time.Now().Unix()
	out := []model{}
	seen := map[string]struct{}{}
	for _, id := range router.DynamicCodingAliases {
		seen[id] = struct{}{}
		out = append(out, model{ID: id, Object: "model", Created: created, OwnedBy: "waired"})
	}
	for _, m := range manifests {
		ids := append([]string{m.ModelID}, m.ModelAliases...)
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, model{ID: id, Object: "model", Created: created, OwnedBy: "waired"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

// handleOpenAIChatCompletions accepts an OpenAI Chat Completions
// request, asks the router which engine to use, and reverse-proxies
// to that engine after rewriting the body's `model` field. SSE
// streams pass through verbatim.
func (h *HandlerSet) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	rr := h.startRequest("openai")
	defer rr.finish()

	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method_not_allowed", "POST only")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8*1024*1024)) // 8MB cap
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "request_too_large", err.Error())
		return
	}
	defer r.Body.Close()

	// Pull the model field out without losing the rest of the JSON
	// body; we'll re-serialise after substitution.
	model, rewritten, err := rewriteModelField(body, "")
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
		return
	}
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "model field is required")
		return
	}

	stickyID := ComputeStickyID(r.Header, body)
	probed, err := h.selectAndProbe(r.Context(), router.Request{Model: model, StickyID: stickyID})
	if err != nil {
		rr.ev.Model = model
		rr.fail(selectionStatus(err), selectionErrorReason(err))
		respondSelectionError(w, err)
		return
	}
	sel := probed.Sel
	rr.setSelection(sel, probed.FallbackFrom, probed.Reason)
	// Release the in-flight slot the Selector held on our behalf.
	// Production Selector always sets a non-nil Release (noopRelease
	// for local/external, tracker.Acquire's release for mesh peers);
	// the nil guard catches test-suite fakes that construct Selection
	// directly. Defer so a panic in the downstream proxy still frees
	// the counter.
	if sel.Release != nil {
		defer sel.Release()
	}

	// Phase 8: surface peer + fallback metadata to the client so
	// claude-code / codex / waired-plugin can show "this request was
	// served by peer-A (fallback from peer-B, reason=capacity_full)".
	setSelectionHeaders(w, sel, probed.FallbackFrom, probed.Reason, h.deps.Recorder)

	// Now rewrite the model field with the engine-specific identifier.
	_, finalBody, err := rewriteModelField(body, sel.EngineModel)
	if err != nil {
		rr.fail(http.StatusInternalServerError, "rewrite_failed")
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "rewrite_failed", err.Error())
		return
	}
	_ = rewritten // kept for symmetry, the second rewrite uses the original body

	adapter, err := h.lookupAdapter(sel)
	if err != nil {
		rr.fail(http.StatusServiceUnavailable, "runtime_unavailable")
		// The raw error names the peer's real DeviceID and overlay IP;
		// agent.log gets the same scrubbed rendering the client does.
		slog.Warn("peer adapter lookup failed", "peer", peerDisplayID(sel), "err", adapterErrorForClient(sel, err))
		writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable", "runtime_unavailable",
			adapterErrorForClient(sel, err))
		return
	}
	if err := adapter.EnsureRunning(r.Context()); err != nil {
		rr.fail(http.StatusServiceUnavailable, "runtime_unhealthy")
		writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable", "runtime_unhealthy", err.Error())
		return
	}

	if err := proxyToEngine(r.Context(), h.clientFor(adapter), adapter.BaseURL(), "/v1/chat/completions", r.Header, finalBody, w); err != nil {
		rr.fail(http.StatusOK, "mid_stream_truncate")
		// Phase 8: if proxying failed AFTER the response headers were
		// sent, HTTP semantics mean we can no longer switch the
		// status — surface the truncation as a slog.Warn so operators
		// see "peer-A died mid-stream" in agent.log even though the
		// client only saw a truncated response.
		slog.Warn("openai proxy truncated mid-stream",
			"err", err,
			"peer", peerDisplayID(sel),
			"model", sel.ModelID,
		)
		return
	}
	rr.succeed()
}

// handleOpenAIResponses returns a 501 in Phase A. The Responses API
// (newer, structured-output OpenAI surface) is intentionally
// deferred to Phase B alongside vision and tool-use parity work.
func (h *HandlerSet) handleOpenAIResponses(w http.ResponseWriter, _ *http.Request) {
	writeOpenAIError(w, http.StatusNotImplemented, "invalid_request_error", "unsupported_endpoint",
		"/v1/responses is not implemented in Phase A; use /v1/chat/completions")
}

// rewriteModelField parses body, captures the existing `model` field,
// and (when newModel != "") replaces it. Returns (existing model
// value, possibly-rewritten body, error). Pass newModel="" to do a
// read-only extract.
func rewriteModelField(body []byte, newModel string) (string, []byte, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", nil, fmt.Errorf("decode body: %w", err)
	}
	var existing string
	if v, ok := raw["model"]; ok {
		_ = json.Unmarshal(v, &existing)
	}
	if newModel == "" {
		return existing, body, nil
	}
	encoded, err := json.Marshal(newModel)
	if err != nil {
		return existing, nil, err
	}
	raw["model"] = encoded
	out, err := json.Marshal(raw)
	if err != nil {
		return existing, nil, err
	}
	return existing, out, nil
}

// proxyToEngine forwards the (already rewritten) body to baseURL+path
// and streams the upstream response back to w. It propagates the
// upstream Content-Type so SSE streams flow correctly.
func proxyToEngine(ctx context.Context, client *http.Client, baseURL, path string, hdr http.Header, body []byte, w http.ResponseWriter) error {
	target, err := url.Parse(baseURL)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "bad_engine_url", err.Error())
		return err
	}
	target.Path = singleSlash(target.Path, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "build_request_failed", err.Error())
		return err
	}
	// Copy a curated set of headers; Authorization is dropped because
	// the local gateway authenticates by listening on loopback only.
	for _, name := range []string{"Content-Type", "Accept", "Accept-Encoding"} {
		if v := hdr.Get(name); v != "" {
			req.Header.Set(name, v)
		}
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "engine_request_failed", err.Error())
		return err
	}
	defer resp.Body.Close()

	// Forward upstream headers (Content-Type especially) so SSE works.
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// singleSlash joins base and tail, collapsing the boundary to one '/'.
func singleSlash(base, tail string) string {
	switch {
	case base == "":
		return tail
	case strings.HasSuffix(base, "/") && strings.HasPrefix(tail, "/"):
		return base + strings.TrimPrefix(tail, "/")
	case !strings.HasSuffix(base, "/") && !strings.HasPrefix(tail, "/"):
		return base + "/" + tail
	default:
		return base + tail
	}
}

// respondSelectionError maps router.Err* sentinels to OpenAI errors.
func respondSelectionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, router.ErrModelNotFound):
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", err.Error())
	case errors.Is(err, router.ErrCapabilityNotMet):
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "capability_not_met", err.Error())
	case errors.Is(err, router.ErrModelNotReady):
		// 503 + Retry-After telegraphs "the model is downloading,
		// please try again". Phase A has the agent pre-pull at
		// startup, so this should be rare in practice.
		w.Header().Set("Retry-After", "30")
		writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable", "model_not_ready", err.Error())
	case errors.Is(err, router.ErrAllPeersOverloaded):
		// Phase 7: every matching mesh peer was at its concurrent-
		// request cap. Retry-After hints the client to back off;
		// the dedicated code lets dashboards tell "underprovisioned
		// mesh" apart from "wrong model".
		w.Header().Set("Retry-After", "5")
		writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable", "waired_all_peers_overloaded", err.Error())
	case errors.Is(err, ErrPeerRoutingDisabled):
		// Phase 8: selectAndProbe surfaces a uniform
		// ErrPeerRoutingDisabled when every probe failed because the
		// listener has PeerAdapterFactory=nil (= overlay-side loop
		// prevention). Map to runtime_unavailable so the operator
		// sees a config / wiring error rather than blaming mesh load.
		writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable", "runtime_unavailable", err.Error())
	case errors.Is(err, router.ErrPinnedPeerUnreachable):
		// An operator-pinned peer is absent / stale / disco-unreachable:
		// environmental, clears when the peer returns — 503, not the
		// default:'s 500.
		w.Header().Set("Retry-After", "5")
		writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable", "waired_pinned_peer_unreachable", err.Error())
	case errors.Is(err, router.ErrHardwareInsufficient):
		writeOpenAIError(w, http.StatusUnprocessableEntity, "invalid_request_error", "hardware_insufficient", err.Error())
	case errors.Is(err, router.ErrRuntimeNotInstalled):
		writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable", "runtime_not_installed", err.Error())
	default:
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "selection_failed", err.Error())
	}
}

func writeOpenAIError(w http.ResponseWriter, status int, errType, code, message string) {
	writeJSON(w, status, openAIErrorEnvelope{Error: OpenAIError{Message: message, Type: errType, Code: code}})
}

// asManifestList is a tiny helper for callers (mostly tests) that
// want to wrap a static slice in the ListManifests function shape.
func asManifestList(manifests []catalog.Manifest) func() []catalog.Manifest {
	return func() []catalog.Manifest { return manifests }
}

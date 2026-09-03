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
	"github.com/waired-ai/waired-agent/internal/runtime"
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
// alias (waired/default — resolved by the router to
// the host's current default, #632), every manifest's model_id, and
// its static aliases are all listed so client SDKs that pre-validate
// the model field accept any spelling.
//
// Each entry also carries max_input_tokens, the same field and the same
// source (Deps.ContextWindowFor) the Anthropic listing stamps — the window
// this host can ACTUALLY serve, not the manifest's native claim (#408).
// The field is not part of OpenAI's model object, and clients that do not
// know it ignore it; the one that needs it is `waired link`, which bakes
// the number into the coding-agent plugins it writes. Without it those
// plugins declared a constant that had nothing to do with the model
// serving, and OpenClaw compacted its context on the first turn (#1001).
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
		// MaxInputTokens is omitted rather than sent as 0 when the window
		// cannot be resolved (no active model, tuning not yet applied):
		// a reader must be able to tell "we do not know" from a real
		// figure, and 0 would look like a real one.
		MaxInputTokens int `json:"max_input_tokens,omitempty"`
	}
	created := time.Now().Unix()
	window := func(id string) int {
		if h.deps.ContextWindowFor == nil {
			return 0
		}
		return h.deps.ContextWindowFor(id)
	}
	out := []model{}
	seen := map[string]struct{}{}
	for _, id := range router.DynamicCodingAliases {
		seen[id] = struct{}{}
		out = append(out, model{ID: id, Object: "model", Created: created, OwnedBy: "waired", MaxInputTokens: window(id)})
	}
	for _, m := range manifests {
		ids := append([]string{m.ModelID}, m.ModelAliases...)
		for _, id := range ids {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, model{ID: id, Object: "model", Created: created, OwnedBy: "waired", MaxInputTokens: window(id)})
		}
	}
	slog.Debug("openai models listed", "count", len(out))
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

// handleOpenAIChatCompletions accepts an OpenAI Chat Completions
// request, asks the router which engine to use, and reverse-proxies
// to that engine after rewriting the body's `model` field. SSE
// streams pass through verbatim.
func (h *HandlerSet) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	rr := h.startRequest(r, "openai")
	defer rr.finish()

	slog.Debug("openai chat request", "method", r.Method, "path", r.URL.Path)

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

	// Decode the top-level members once, and read both things this
	// handler needs out of that one pass: the model field, and the
	// conversation identity the sticky id is built from. Every value
	// stays a json.RawMessage, so nothing here walks the conversation.
	raw, err := decodeJSONObject(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
		return
	}
	model := jsonStringMember(raw, "model")
	if model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "missing_model", "model field is required")
		return
	}

	// Sticky id from what the CLIENT sent, deliberately before the fold
	// below: the id is a conversation identity, and a peer that upgrades
	// mid-conversation must not have its affinity move under it because
	// our normalisation started or stopped applying.
	stickyID := ComputeStickyID(r.Header, body, stickyIdentityFromOpenAIBody(raw))

	// waired-agent#1055: fold a mid-conversation system / developer turn
	// into the leading system message, as AnthropicToOpenAI already does
	// for the other surface (waired-agent#1035).
	//
	// After the sticky id, before the window guard: from here on `body`
	// is what the engine will receive, so CountOpenAIPromptTokensApprox
	// below counts the bytes actually forwarded. That is the #436
	// invariant — the requesting node and the serving peer must not
	// disagree about the size of one conversation — and on the Anthropic
	// side the requester has counted the folded body since #1054.
	//
	// The prompt-cache objection that kept this off the native surface
	// does not survive contact with rewriteModelField: it decodes into a
	// map and re-marshals, and Go sorts map keys, so the client's key
	// order and whitespace are already gone by the time anything reaches
	// an engine. What the cache needs is that an unchanged conversation
	// produces unchanged bytes turn after turn, and a fold that is a
	// no-op on an already-legal conversation preserves exactly that.
	if folded, changed := normalizeOpenAIBodyInstructionTurns(body); changed {
		slog.Debug("openai instruction turns folded into the leading system message",
			"model", model, "bytes_before", len(body), "bytes_after", len(folded))
		body = folded
	}

	// No capacity queue on this leg (waired-agent#786 arms one only for
	// the Claude surface). `waired infer` sends one request at a time, so
	// there are no concurrent sub-requests to pace, and this same handler
	// serves the mesh-ingress leg — where holding the peer's caller open
	// would move the wait onto a machine that cannot see why.
	probed, err := h.selectAndProbe(r.Context(), router.Request{Model: model, StickyID: stickyID}, 0)
	if err != nil {
		rr.ev.Model = model
		rr.failSelection(err, selectionStatus(err))
		respondSelectionError(w, err)
		return
	}
	sel := probed.Sel
	rr.setSelection(sel, probed.FallbackFrom, probed.Reason)
	slog.Debug("openai dispatch",
		"model", model,
		"engine_model", sel.EngineModel,
		"mode", sel.ExecutionMode,
		"peer", peerDisplayID(sel),
		"fallback_from", probed.FallbackFrom,
	)
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

	// #623 context-window guard. On the overlay listener this is the
	// SERVING side (waired-agent#436): the requesting node holds its own
	// copy of the check, sized from the window this peer advertised, but
	// an advertisement is a snapshot and a re-tune between the push and
	// the request leaves the requester guarding against a window this
	// engine no longer serves. Without a check here that prompt reaches
	// ollama and loses its head silently, which is the failure #623
	// exists to prevent.
	//
	// effectiveContextWindow rather than Deps.ContextWindowFor directly:
	// the loopback and data-plane surfaces can dispatch to a peer, and
	// ContextWindowFor knows only this device — its manifests, its
	// applied tuning. Guarding a peer-bound request against the local
	// window refuses prompts the peer could serve when this device holds
	// the smaller model, and passes prompts the peer cannot when it holds
	// the larger. Selection.ContextWindow is what the chosen peer says it
	// serves and wins whenever it is set. The overlay's selection is
	// local by construction, so it reads 0 there and this falls back to
	// what this engine is actually sized for right now — the quantity a
	// truncation depends on, and the right one for the serving half.
	// 0 means "unknown" and fails open.
	if win := effectiveContextWindow(h.deps, sel); win > 0 {
		if n := CountOpenAIPromptTokensApprox(body); n > win {
			rr.fail(http.StatusBadRequest, "context_overflow")
			slog.Debug("openai context overflow", "model", sel.ModelID, "tokens", n, "window", win)
			// The header is what lets a requesting waired node turn this
			// into the Anthropic 400 its client compacts on; a plain
			// OpenAI error would reach Claude Code as an upstream fault.
			w.Header().Set(HeaderLocalError, LocalErrorContextOverflow)
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "context_length_exceeded",
				fmt.Sprintf("prompt is too long: %d tokens > %d maximum", n, win))
			return
		}
	}

	// Now rewrite the model field with the engine-specific identifier.
	_, finalBody, err := rewriteModelField(body, sel.EngineModel)
	if err != nil {
		rr.fail(http.StatusInternalServerError, "rewrite_failed")
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "rewrite_failed", err.Error())
		return
	}

	// Hold a slot on the shared admission counter for as long as this
	// request occupies the local engine — engine start included (§8.2).
	defer h.admitLocalEngine(r.Context(), sel)()

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
		slog.Debug("openai engine not running", "runtime", displayRuntime(sel))
		writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable", "runtime_unhealthy", err.Error())
		return
	}

	started, err := proxyToEngine(r.Context(), h.clientFor(adapter), adapter.BaseURL(), "/v1/chat/completions", r.Header, finalBody, w, sel, rr, asFailureReporter(adapter))
	if err != nil {
		if !started {
			// The request never reached the engine. proxyToEngine chose
			// and wrote the status the client received and recorded that
			// same status on rr, so there is no truncation to describe
			// here and nothing to restate (waired-agent#538).
			slog.Warn("openai proxy failed before the engine answered",
				"err", adapterErrorForClient(sel, err),
				"peer", peerDisplayID(sel),
				"model", sel.ModelID,
			)
			return
		}
		reason := engineLegReason(r.Context(), "mid_stream_truncate")
		rr.fail(http.StatusOK, reason)
		// Phase 8: proxying failed AFTER the response headers were
		// sent, and HTTP semantics mean we can no longer switch the
		// status — surface the truncation as a slog.Warn so operators
		// see "peer-A died mid-stream" in agent.log even though the
		// client only saw a truncated response.
		slog.Warn("openai proxy truncated mid-stream",
			"reason", reason,
			"err", adapterErrorForClient(sel, err),
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

// decodeJSONObject decodes a request body into its top-level members,
// leaving every value as raw bytes. Callers that want one member pay
// for one pass over the document and nothing more.
func decodeJSONObject(body []byte) (map[string]json.RawMessage, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode body: %w", err)
	}
	return raw, nil
}

// jsonStringMember reads one member as a string. Absent, null or a
// non-string member reads as "" — no claim, not an empty claim.
func jsonStringMember(raw map[string]json.RawMessage, name string) string {
	v, ok := raw[name]
	if !ok {
		return ""
	}
	var s string
	_ = json.Unmarshal(v, &s)
	return s
}

// rewriteModelField parses body, captures the existing `model` field,
// and (when newModel != "") replaces it. Returns (existing model
// value, possibly-rewritten body, error). Pass newModel="" to do a
// read-only extract.
func rewriteModelField(body []byte, newModel string) (string, []byte, error) {
	raw, err := decodeJSONObject(body)
	if err != nil {
		return "", nil, err
	}
	existing := jsonStringMember(raw, "model")
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
// rr may be nil: proxyToEngine is also called from paths that keep no
// telemetry record. When non-nil, a passive sniffer reads the token
// counts out of the bytes being forwarded (waired#829) — see
// usageSniffer for why this is a tee and not a buffer.
//
// responseStarted says whether the engine's response had begun reaching
// the client when err was returned, and it decides who owns the failure
// (waired-agent#538). Until it has, the status is still proxyToEngine's
// to choose: it writes the error the client receives and records that
// same status and reason on rr, exactly as the non-2xx branch below
// already did. Once it has, the status is spent, and only the caller's
// mid-stream reason is left to record.
//
// sel is what the request was dispatched to, and is here only so
// adapterErrorForClient can render an error without the peer's overlay
// address in it — every error below carries the URL it was dialling.
//
// engineErrorSniffMax bounds how much of a non-2xx engine body is read
// before forwarding, so the adapter can classify it without buffering an
// arbitrarily large error.
const engineErrorSniffMax = 32 << 10

func proxyToEngine(ctx context.Context, client *http.Client, baseURL, path string, hdr http.Header, body []byte, w http.ResponseWriter, sel router.Selection, rr *requestRec, reporter runtime.FailureReporter) (responseStarted bool, err error) {
	target, err := url.Parse(baseURL)
	if err != nil {
		rr.fail(http.StatusInternalServerError, "bad_engine_url")
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "bad_engine_url", adapterErrorForClient(sel, err))
		return false, err
	}
	target.Path = singleSlash(target.Path, path)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		rr.fail(http.StatusInternalServerError, "build_request_failed")
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", "build_request_failed", adapterErrorForClient(sel, err))
		return false, err
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

	// baseURL / host is deliberately not logged: for a remote leg it is the
	// peer's overlay address, which must never reach a log line (spec §8.5).
	slog.Debug("gateway upstream request", "path", target.Path, "body_bytes", len(body))
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		// The status and reason the two Anthropic legs have always used
		// for this exit (anthropic.go's non-streaming and streaming
		// twins): one failure described two ways by two transports is how
		// they drift. Recording it is also what keeps emitUsage's
		// status-only gate honest — nothing reached an engine, so there
		// is nothing to meter (waired-agent#538).
		// engineLegReason separates the client's own departure from the
		// engine's failure — the same split the Anthropic legs make, kept
		// here for the same reason the paragraph above gives.
		rr.fail(http.StatusBadGateway, engineLegReason(ctx, "engine_request_failed"))
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "engine_request_failed", adapterErrorForClient(sel, err))
		return false, err
	}
	defer resp.Body.Close()
	slog.Debug("gateway upstream response",
		"status", resp.StatusCode,
		"ttfb_ms", time.Since(start).Milliseconds(),
		"content_type", resp.Header.Get("Content-Type"),
	)

	// Forward upstream headers (Content-Type especially) so SSE works.
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	if resp.StatusCode/100 != 2 {
		// The engine refused. Two things must happen that used not to
		// (waired-agent#29):
		//
		//  1. Tell the adapter. Only it knows its engine's error
		//     vocabulary, and a body naming a dead model runner is the
		//     ONLY signal that separates "engine broken" from "bad
		//     request" — the parent `ollama serve` keeps answering
		//     /api/tags with 200 after its llama-server child dies.
		//  2. Record the real status. This path used to fall through to
		//     the caller's rr.succeed(), which logged 200 for a wire-500
		//     in the event ring AND counted a failed request as a
		//     billable usage sample.
		//
		// The body is read HEAD-first so the reporter can classify it,
		// then forwarded verbatim — including anything past the sniff cap,
		// so a large upstream error still reaches the client intact.
		head, _ := io.ReadAll(io.LimitReader(resp.Body, engineErrorSniffMax))
		if reporter != nil {
			reporter.ReportUpstreamFailure(resp.StatusCode, head)
		}
		// A deterministic request-shape rejection is not a transient
		// upstream fault, and saying 5xx invites the retry storm
		// waired-agent#1035 measured — 11 attempts over 182 s against a
		// rejection that would have failed identically on attempt 12.
		// The Anthropic legs have said 400 to it since #1054; this one
		// is the same engine and the same body.
		//
		// The reporter has already seen the engine's OWN status above,
		// so the dead-runner and out-of-memory classifications are
		// untouched by the number the client is given.
		if clientStatus, errType, reason := classifyEngineFailure(resp.StatusCode, head); clientStatus != resp.StatusCode {
			// The engine's headers were copied onto w a few lines up.
			// A rewritten body must not inherit its length or its
			// encoding; writeJSON replaces the content type.
			w.Header().Del("Content-Length")
			w.Header().Del("Content-Encoding")
			rr.fail(clientStatus, reason)
			writeOpenAIError(w, clientStatus, errType, reason, string(head))
			return true, nil
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(head)
		_, _ = io.Copy(w, resp.Body)
		rr.fail(resp.StatusCode, "engine_error")
		return true, nil
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	sniff := newUsageSniffer(resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"))
	// Record whatever was observed on every exit, including a truncated
	// stream: the engine still did the work the client partially
	// received, and a usage chunk may already have arrived.
	defer func() {
		if in, out, ok := sniff.Usage(); ok {
			rr.setUsage(in, out)
			// After Usage(), which is what decodes a non-SSE body.
			rr.setCachedInput(sniff.CachedInput())
		}
	}()
	buf := make([]byte, 16*1024)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			// Forward first, meter second: the client's bytes are never
			// delayed by, or dependent on, the sniffer.
			if _, werr := w.Write(buf[:n]); werr != nil {
				return true, werr
			}
			if flusher != nil {
				flusher.Flush()
			}
			sniff.Feed(buf[:n])
		}
		if rerr == io.EOF {
			return true, nil
		}
		if rerr != nil {
			return true, rerr
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
	case router.BelowModelSizeFloor(err):
		// FIRST, because the operator's own floor outranks every reason
		// below it (waired-agent#1128). On an engine-less requester the
		// same miss arrives as ErrLocalInferenceOff — that toggle is
		// normally what removed the local fallback, but here it removed
		// nothing: the mesh had candidates and the floor excluded them.
		// Saying "local inference disabled" sends the operator to the
		// wrong switch, and "model_not_served" sends them looking for a
		// broken peer.
		w.Header().Set(HeaderLocalError, LocalErrorModelTooSmall)
		if floor := router.ModelSizeFloor(err); floor != "" {
			w.Header().Set(HeaderMinModelSize, floor)
		}
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", err.Error())
	case errors.Is(err, router.ErrModelNotFound):
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", err.Error())
	case errors.Is(err, router.ErrCapabilityNotMet):
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", "capability_not_met", err.Error())
	case errors.Is(err, router.ErrLocalInferenceOff):
		// Local inference is off AND the mesh had nothing — the only
		// case the old outermost gate was right about (waired-agent#829).
		// Same body it wrote, so a client that learned this error keeps
		// reading it; what changed is that the request now had to fail
		// routing to get here.
		w.Header().Set(HeaderLocalError, LocalErrorInferenceDisabled)
		writeInferenceDisabled(w)
	case errors.Is(err, router.ErrModelNotReady):
		if router.ModelIsArriving(err) {
			// 503 + Retry-After telegraphs "the model is downloading,
			// please try again". Phase A has the agent pre-pull at
			// startup, so this should be rare in practice.
			w.Header().Set("Retry-After", "30")
			writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable", "model_not_ready", err.Error())
			return
		}
		// Nothing is fetching it, so "try again" is advice that never
		// comes true — see the Anthropic twin (waired-agent#788).
		w.Header().Set(HeaderLocalError, LocalErrorModelNotServed)
		writeOpenAIError(w, http.StatusNotFound, "invalid_request_error", "model_not_served", err.Error())
	case errors.Is(err, router.ErrAllPeersOverloaded):
		// Phase 7: every matching mesh peer was at its concurrent-
		// request cap. Retry-After hints the client to back off;
		// the dedicated code lets dashboards tell "underprovisioned
		// mesh" apart from "wrong model".
		w.Header().Set("Retry-After", "5")
		writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable", "waired_all_peers_overloaded", err.Error())
	case errors.Is(err, router.ErrPeersDidNotAnswer):
		// Matching peers existed but none answered its readiness probe,
		// so nothing is known about the mesh's load. Its own code, because
		// reporting this as waired_all_peers_overloaded points the reader
		// at capacity — the one thing the gateway did not measure
		// (waired-agent#624).
		w.Header().Set("Retry-After", "5")
		writeOpenAIError(w, http.StatusServiceUnavailable, "service_unavailable", "waired_peers_did_not_answer", err.Error())
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
		// default:'s 500. Naming the peer keeps the general surface's
		// diagnosis as good as the Claude one's.
		if peer := pinnedPeerOf(err); peer != "" {
			w.Header().Set(HeaderInferencePeer, peer)
		}
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

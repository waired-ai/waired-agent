package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// anthropicErrorEnvelope mirrors Anthropic's JSON error shape.
type anthropicErrorEnvelope struct {
	Type  string                `json:"type"`
	Error anthropicErrorPayload `json:"error"`
}

type anthropicErrorPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, anthropicErrorEnvelope{
		Type:  "error",
		Error: anthropicErrorPayload{Type: errType, Message: message},
	})
}

// classifyEngineFailure decides what a non-2xx engine reply becomes on
// the wire: the status the client is given, the Anthropic error type,
// and the observability reason.
//
// Everything passes through at the engine's own status EXCEPT a
// deterministic request-shape rejection (waired-agent#1035), which
// becomes a 400. A 500 tells a well-behaved client "transient, try
// again" — Claude Code retried one 11 times over 182 s against a
// rejection that would have failed identically on attempt 12. Saying 400
// is both the accurate statement and the one that stops the storm; auto
// mode still reroutes, because the intercept's fallback window is any
// status >= 400.
//
// Matched on the marker regardless of which status the engine picked:
// the classification is about why the request failed, not about the
// number in front of it.
func classifyEngineFailure(status int, body []byte) (clientStatus int, errType, reason string) {
	if IsEngineRequestShapeRejection(string(body)) {
		return http.StatusBadRequest, "invalid_request_error", "engine_request_shape"
	}
	return status, "upstream_error", "upstream_error"
}

// handleAnthropicMessages overrides the stub in server.go. It accepts
// an Anthropic Messages request, translates to OpenAI Chat Completions
// (preserving Anthropic semantics where they diverge — see
// docs/knowledges/20260502.md), proxies to the selected engine, then
// translates the response (or stream) back to Anthropic's wire shape.
func (h *HandlerSet) handleAnthropicMessagesImpl(w http.ResponseWriter, r *http.Request) {
	rr := h.startRequest(r, "anthropic")
	defer rr.finish()

	slog.Debug("anthropic messages request", "method", r.Method, "path", r.URL.Path)

	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "POST only")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8*1024*1024))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	defer r.Body.Close()

	var req AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "malformed JSON: "+err.Error())
		return
	}
	if req.Model == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model is required")
		return
	}

	// Reject explicitly-unsupported features early so the user gets
	// a clean 400 rather than a confusing engine error.
	if hasMetadataFeature(req.Metadata, "cache_control") {
		writeAnthropicError(w, http.StatusBadRequest, "unsupported_feature", "cache_control is Phase B")
		return
	}

	openaiReq, err := AnthropicToOpenAI(req)
	if err != nil {
		var unsup *ErrUnsupportedFeature
		if errors.As(err, &unsup) {
			writeAnthropicError(w, http.StatusBadRequest, "unsupported_feature", unsup.Error())
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	// Traffic class (#645): derived from the ORIGINAL client model id
	// before any remap, because ResolveUnknownModel would erase the
	// waired/subagent marker. Folded into the sticky id so the main and
	// subagent legs of one conversation keep separate peer affinity.
	class := ""
	if h.deps.ClassifyModel != nil {
		class = h.deps.ClassifyModel(req.Model)
	}
	stickyID := ComputeStickyID(r.Header, body, stickyIdentityFromAnthropic(req))
	if stickyID != "" && class != "" {
		stickyID += ":" + class
	}
	rr.ev.Class = class
	routeReq := router.Request{Model: req.Model, StickyID: stickyID, Class: class}
	// waired#1031: a /model tier obliges the serving endpoint to declare
	// the window Claude Code already sized this session to. Derived from
	// the client's ORIGINAL id — ResolveUnknownModel below rewrites the id
	// to something servable, and the promise belongs to the id the user
	// picked, not to whatever it resolved to. Gated on the same flag that
	// advertises the ids at all, so a deployment with directives off never
	// grows a filter it cannot have asked for.
	if h.deps.ClaudeModelDirectives {
		// The tier is read from the id AND from `anthropic-beta`, because
		// Claude Code strips "[1m]" before sending and the header is the only
		// place a 1M session still declares itself (waired-agent#1036).
		routeReq.MinContextWindow = RequiredWindowForRequest(req.Model, r.Header.Values("Anthropic-Beta"))
		// Same seat, same reason: a directive that names a node has to
		// survive the ResolveUnknownModel rewrite below, and the choice
		// belongs to the id the user picked in /model.
		routeReq.NodeDirective = NodeDirectiveFor(req.Model)
	}
	capacityWait := capacityQueueBudget(h.deps, r, class)
	probed, err := h.selectAndProbe(r.Context(), routeReq, capacityWait)
	if errors.Is(err, router.ErrModelNotFound) && h.deps.ResolveUnknownModel != nil {
		// Claude-intercept model mapping (#600): the Anthropic ids Claude
		// Code sends never exist in the catalog, so an alias miss resolves
		// to a served model — the class's target-node model under the
		// per-class policy (#647), the device-active model otherwise.
		// Selection is retried with the mapped id; the response body
		// keeps echoing the client's original id.
		mapped, ok := h.deps.ResolveUnknownModel(req.Model, class)
		if !ok {
			rr.ev.Model = req.Model
			rr.fail(http.StatusServiceUnavailable, "no_local_model")
			w.Header().Set(HeaderLocalError, "no_model")
			writeAnthropicError(w, http.StatusServiceUnavailable, "waired_no_local_model",
				fmt.Sprintf("waired: no local model is active on this device to serve %q — check `waired status` and `waired models ls`", req.Model))
			return
		}
		routeReq.Model = mapped
		slog.Debug("anthropic model mapped", "requested", req.Model, "mapped", mapped, "class", class)
		probed, err = h.selectAndProbe(r.Context(), routeReq, capacityWait)
	}
	if err != nil {
		rr.ev.Model = routeReq.Model // the mapped id when mapping was applied
		rr.failSelection(err, anthropicSelectionStatus(err))
		respondAnthropicSelectionError(w, err, probed.queuedFor)
		return
	}
	sel := probed.Sel
	rr.setSelection(sel, probed.FallbackFrom, probed.Reason)
	// Release the in-flight slot the Selector held on our behalf.
	// See handleOpenAIChatCompletions for the nil-guard rationale —
	// production paths always set Release, test fakes may not.
	if sel.Release != nil {
		defer sel.Release()
	}
	// Phase 8: surface fallback metadata so claude-code / waired-plugin
	// can show which peer served the request and why a fallback fired.
	setSelectionHeaders(w, sel, probed.FallbackFrom, probed.Reason, h.deps.Recorder)
	// The catalog id that ANSWERED, on every successful selection. This
	// used to sit inside the ResolveUnknownModel branch above, so it was
	// set only for the ids Claude Code cannot resolve itself (#600). The
	// Claude intercept's last-served record fires on this header, which
	// left every request naming a catalog id directly — local or
	// peer-served — unrecorded while observability recorded it correctly
	// (#755). Read from sel rather than the requested id so a router
	// fallback to another model reports the model that really answered.
	if sel.ModelID != "" {
		w.Header().Set(HeaderLocalModel, sel.ModelID)
	}
	openaiReq.Model = sel.EngineModel
	// A request that did not ask for extended thinking must not get a
	// reasoning trace. Applied here rather than in the conversion
	// because the dialect depends on the engine, which only the
	// selection knows (#856). The background calls a coding agent makes
	// — naming a session and the like — are the ones that send no
	// thinking config at all, and on a single-slot engine their
	// reasoning is what the user's first turn queues behind.
	if ThinkingDisabled(req.Thinking) {
		ApplyThinkingControl(&openaiReq, sel.Runtime)
	}
	slog.Debug("anthropic dispatch",
		"model", req.Model,
		"engine_model", sel.EngineModel,
		"mode", sel.ExecutionMode,
		"peer", peerDisplayID(sel),
		"fallback_from", probed.FallbackFrom,
		"stream", req.Stream,
		"class", class,
	)

	// #623 context-window guard: reject a prompt that overruns the served
	// model's effective window with the exact Anthropic 400 that triggers
	// Claude Code's auto-compaction, instead of forwarding it to the engine
	// (Ollama would silently truncate the prompt head — the root cause of
	// local-model tool spam / instruction drift). Placed before the engine
	// is looked up / started so an over-window request never loads a model.
	// The staged HeaderLocalError marks this 400 as "surface, don't fall
	// back" for the intercept's auto mode (a fallback to the real Anthropic
	// API would abandon local serving instead of compacting). The guard is
	// active only where Deps.ContextWindowFor is wired (the Claude-intercept
	// HandlerSet); a 0 window means "unknown" and fails open.
	//
	// The window belongs to whoever ANSWERS. Deps.ContextWindowFor knows
	// only this device — its manifests, its applied tuning — so on a mesh
	// leg it used to guard the request against the wrong engine's window
	// and let an over-window prompt through to be truncated at the head
	// (waired-agent#436). Selection.ContextWindow is what the chosen peer
	// says it serves and wins whenever it is set; a peer that declares
	// nothing (0 — including every agent predating the field) falls back
	// to the local computation, which is what this did before.
	// Encoded here rather than after the engine checks below: the guard
	// counts the bytes it is about to forward, which is the same body
	// the serving side counts on a mesh leg, and an unencodable request
	// must not start an engine either.
	encoded, err := json.Marshal(openaiReq)
	if err != nil {
		rr.fail(http.StatusInternalServerError, "encode_failed")
		writeAnthropicError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	// Counted once, here, and kept: the over-window guard below needs it,
	// and so does the prefill observation this request is about to make
	// (waired-agent#1127). It used to be computed inside the guard and
	// dropped, so the one place that knows how many tokens went to the
	// peer and the one place that knows how long the peer took were
	// different scopes.
	rr.promptTokens = CountOpenAIPromptTokensApprox(encoded)
	if win := effectiveContextWindow(h.deps, sel); win > 0 {
		if n := rr.promptTokens; n > win {
			rr.fail(http.StatusBadRequest, "context_overflow")
			slog.Debug("anthropic context overflow",
				"model", sel.ModelID, "tokens", n, "window", win,
				"peer", peerDisplayID(sel), "declared", sel.ContextWindow > 0)
			w.Header().Set(HeaderLocalError, LocalErrorContextOverflow)
			writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
				fmt.Sprintf("prompt is too long: %d tokens > %d maximum", n, win))
			return
		}
	}

	// What this device's engine was doing when the request arrived
	// (waired-agent#837). Deliberately ABOVE the admission slot below: the
	// count is "what this request queued behind", so taking it afterwards
	// would include this request and read 1 on an idle machine.
	residency, inflight := h.localEngineFacts(sel)
	rr.setEngineFacts(residencyVerdict(residency, sel.EngineModel), inflight)

	// Hold a slot on the shared admission counter for as long as this
	// request occupies the local engine — engine start included (§8.2).
	defer h.admitLocalEngine(r.Context(), sel)()

	adapter, err := h.lookupAdapter(sel)
	if err != nil {
		rr.fail(http.StatusServiceUnavailable, "runtime_unavailable")
		// The raw error names the peer's real DeviceID and overlay IP;
		// agent.log gets the same scrubbed rendering the client does.
		slog.Warn("peer adapter lookup failed", "peer", peerDisplayID(sel), "err", adapterErrorForClient(sel, err))
		writeAnthropicError(w, http.StatusServiceUnavailable, "runtime_unavailable", adapterErrorForClient(sel, err))
		return
	}
	if err := adapter.EnsureRunning(r.Context()); err != nil {
		rr.fail(http.StatusServiceUnavailable, "runtime_unhealthy")
		slog.Debug("anthropic engine not running", "runtime", displayRuntime(sel))
		writeAnthropicError(w, http.StatusServiceUnavailable, "runtime_unhealthy", err.Error())
		return
	}

	rr.succeed()
	client := h.clientFor(adapter)
	if req.Stream {
		h.proxyAnthropicStream(r.Context(), client, adapter.BaseURL(), encoded, req.Model, req.Tools, w,
			waitPolicyFor(h.deps, sel, class), sel, rr, asFailureReporter(adapter))
		return
	}
	h.proxyAnthropicNonStream(r.Context(), client, adapter.BaseURL(), encoded, req.Model, req.Tools, w, sel, rr, asFailureReporter(adapter))
}

// handleAnthropicCountTokensImpl returns an approximate token count.
// The X-Waired-Token-Count: approximate header tells callers it isn't
// the model's real tokeniser (Phase B will replace this with either
// an Ollama tokenize round-trip or a manifest-supplied tokeniser).
func (h *HandlerSet) handleAnthropicCountTokensImpl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "POST only")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1*1024*1024))
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	defer r.Body.Close()

	var req AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "malformed JSON: "+err.Error())
		return
	}
	w.Header().Set("X-Waired-Token-Count", "approximate")
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": CountTokensApprox(req)})
}

// rr may be nil (direct calls from tests). The upstream response is
// already decoded here, so metering costs one field read (waired#829).
// sel is here for the same reason as on proxyToEngine: a transport error
// names the URL it was dialling, and for a Public Share peer that is an
// overlay address no client and no log line may carry (spec §8.5).
func (h *HandlerSet) proxyAnthropicNonStream(ctx context.Context, client *http.Client, baseURL string, body []byte, originalModel string, offered []AnthropicTool, w http.ResponseWriter, sel router.Selection, rr *requestRec, reporter runtime.FailureReporter) {
	start := time.Now()
	var (
		resp     *http.Response
		respBody []byte
	)
	// Retried on the same condition as the streaming path, for the same
	// reason: the engine rejecting the model's own tool syntax is a bad
	// draw, and the next draw is independent (#442). Nothing has been
	// sent yet on this path, so there is no commitment to reason about —
	// the whole turn is still in hand.
	//
	// Kept in step with the streaming path deliberately. The probe drives
	// both and #440 pools the results as two samples of one thing; a
	// retry on only one transport would quietly make that untrue.
	for attempt := 1; ; attempt++ {
		var err error
		resp, err = h.postToEngine(ctx, client, baseURL, "/v1/chat/completions", body)
		if err != nil {
			reason := engineLegReason(ctx, "engine_request_failed")
			rr.fail(http.StatusBadGateway, reason)
			// Staged so the intercept's journal reads local_client_disconnected
			// rather than local_status_502 — the status alone cannot tell the
			// two apart, and 502 is what put waired-agent#1168 on the engine.
			w.Header().Set(HeaderLocalError, reason)
			// Warn, not Debug, and carrying the error: before this the branch
			// logged only a latency, so nothing anywhere recorded WHY a leg
			// failed. "context canceled" here is the whole diagnosis.
			slog.Warn("gateway: the engine leg failed before any response headers",
				"reason", reason, "err", adapterErrorForClient(sel, err),
				"model", recordedModel(rr), "latency_ms", time.Since(start).Milliseconds())
			writeAnthropicError(w, http.StatusBadGateway, "upstream_error", adapterErrorForClient(sel, err))
			return
		}
		respBody, err = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			rr.fail(http.StatusBadGateway, "engine_read_failed")
			writeAnthropicError(w, http.StatusBadGateway, "upstream_error", adapterErrorForClient(sel, err))
			return
		}
		if resp.StatusCode/100 == 2 || attempt > maxStreamRetries ||
			!IsEngineParseFailure(string(respBody)) {
			break
		}
		slog.Warn("gateway: engine could not parse the tool call the model emitted; retrying",
			"model", recordedModel(rr), "attempt", attempt,
			"max_attempts", maxStreamRetries+1, "status", resp.StatusCode)
	}
	slog.Debug("anthropic upstream response", "status", resp.StatusCode, "latency_ms", time.Since(start).Milliseconds())
	if resp.StatusCode/100 != 2 {
		// Pass through upstream's error verbatim, wrapping it in our
		// envelope so clients still see Anthropic-shaped errors.
		// Tell the adapter too: on this surface the Claude intercept
		// discards the error before replaying upstream, so nobody else
		// ever learns the engine died (waired-agent#29).
		if relayPeerContextOverflow(w, resp, respBody, rr) {
			return
		}
		// reportEngineFailure gets the engine's OWN status, never the
		// remapped one: it is the component whose job is to judge its own
		// engine, and ReportUpstreamFailure returns early below 500, so a
		// remapped 400 would silently skip its canary.
		reportEngineFailure(reporter, resp.StatusCode, respBody)
		status, errType, reason := classifyEngineFailure(resp.StatusCode, respBody)
		if status != resp.StatusCode {
			slog.Warn("gateway: engine refused the request shape; surfacing 400 so the client stops retrying",
				"model", recordedModel(rr), "engine_status", resp.StatusCode)
			w.Header().Set(HeaderLocalError, LocalErrorEngineRequestShape)
		}
		rr.fail(status, reason)
		writeAnthropicError(w, status, errType, strings.TrimSpace(string(respBody)))
		return
	}
	var openaiResp OpenAIResponse
	if err := json.Unmarshal(respBody, &openaiResp); err != nil {
		rr.fail(http.StatusBadGateway, "malformed_engine_response")
		writeAnthropicError(w, http.StatusBadGateway, "upstream_error", "malformed engine response: "+err.Error())
		return
	}
	rr.setUsage(int64(openaiResp.Usage.PromptTokens), int64(openaiResp.Usage.CompletionTokens))
	rr.setCachedInput(int64(openaiResp.Usage.CachedPromptTokens()))
	out := OpenAIToAnthropic(openaiResp, originalModel, offered)
	reportReasoningLeak(rr, visibleText(out))
	if out.ToolRecovery != "" {
		rr.setToolRecovery(out.ToolRecovery)
		logToolRecovery(out.ToolRecovery, recoveredToolName(out), originalModel, false)
	}
	writeJSON(w, http.StatusOK, out)
}

// recoveredToolName returns the name of the tool_use block the recovery
// synthesised. Only called once ToolRecovery is set, and the block is
// appended last, so the final tool_use is the recovered one.
func recoveredToolName(resp AnthropicResponse) string {
	for i := len(resp.Content) - 1; i >= 0; i-- {
		if resp.Content[i].Type == "tool_use" {
			return resp.Content[i].Name
		}
	}
	return ""
}

// logToolRecovery records that the gateway put back a call the engine
// dropped. The tool NAME and the dialect are logged; the fragment and
// the surrounding assistant text are not, because they are message
// content and no gateway log line may carry it (spec §8.5 / §15-10 —
// the same rule that keeps peer base URLs and prompts out of these
// lines).
func logToolRecovery(shape, tool, model string, streaming bool) {
	slog.Warn("gateway: recovered a tool call the engine left in the assistant text",
		"shape", shape, "tool", tool, "model", model, "stream", streaming)
}

// reportReasoningLeak logs when visible assistant text carries a
// reasoning channel marker.
//
// Reported, never acted on: a turn that leaked its trace and answered
// still answered. Until this, nothing in the product looked for the
// leak at all — the only detector lived in a dev script read by nobody
// in CI (scripts/dev/agentgrade-contract.py), and the leak reached real
// users (internal/e2e/agentgrade/hold_test.go).
func reportReasoningLeak(rr *requestRec, text string) {
	if text == "" || !textLeaksReasoning(text) {
		return
	}
	slog.Warn("gateway: the engine left a reasoning trace in visible text",
		"model", recordedModel(rr), "bytes", len(text))
}

// visibleText concatenates a converted response's text blocks.
func visibleText(resp AnthropicResponse) string {
	var b strings.Builder
	for _, blk := range resp.Content {
		if blk.Type == "text" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// partialTool accumulates one in-flight streamed tool call.
type partialTool struct {
	ID, Name string
	Args     bytes.Buffer
}

// streamToolCallDelta is one tool_calls entry on the STREAMING surface.
// It differs from OpenAIToolCall by carrying `index`, which identifies
// which call in the turn the delta belongs to. A pointer so an engine
// that omits the field is distinguishable from one that sent index 0.
type streamToolCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toolDeltaKey decides which in-flight tool call a streamed delta
// belongs to.
//
// This was previously `key := len(tools)`, which made EVERY delta after
// the first start a new partial: a call whose arguments arrive split
// across chunks — the normal case, since engines stream the argument
// JSON token by token — was emitted as several tool_use blocks each
// holding a fragment of the JSON. Nothing caught it because the
// streaming tool-call path had no test at all (added in #409).
//
// `index` is authoritative when present; it is what the OpenAI streaming
// schema defines the field for. Without it, an `id` identifies the call,
// and a delta carrying neither is a continuation of the one in flight.
func toolDeltaKey(index *int, id string, tools map[int]*partialTool, order []int, next *int) int {
	if index != nil {
		return *index
	}
	if id != "" {
		for _, k := range order {
			if tools[k].ID == id {
				return k
			}
		}
		key := *next
		*next++
		return key
	}
	if len(order) > 0 {
		return order[len(order)-1]
	}
	key := *next
	*next++
	return key
}

// waitPolicy is what a streaming leg may do while the engine has produced
// nothing at all.
//
// At most one of Budget and Keepalive is ever set, and that exclusivity is
// load-bearing rather than tidy. Since the retirement of the auto crossing
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md)
// the split is by LEG, not by permission: a peer leg stays uncommitted so the
// liveness watch can still end the turn with a status that names the peer,
// and a local leg writes keepalive frames so the wait is not silent — after
// which its status is fixed and a later failure can only be in-band.
type waitPolicy struct {
	// Budget is the pre-commit deadline; 0 waits indefinitely.
	Budget time.Duration
	// Reason is the HeaderLocalError value staged when Budget fires.
	Reason string
	// Keepalive is the interval between SSE keepalive frames while the
	// engine has produced nothing; 0 writes none.
	Keepalive time.Duration
	// Liveness, when non-nil, replaces Budget's flat deadline with a watch
	// on the serving peer: Budget becomes the grace period before the
	// first health check, and the wait then continues for as long as that
	// peer says it is working (waired-agent#1040, peerwait.go). It is set
	// only on a leg that already had a Budget, so the "never armed
	// together with Keepalive" rule above is unchanged.
	Liveness *peerLiveness
}

// waitPolicyFor decides what this streaming leg may do before the engine's
// first byte (#757, waired-agent#837, waired-agent#1040).
//
// Two cases, by leg:
//
//   - a PEER leg waits on the peer itself, not on a stopwatch: Budget is the
//     grace period before the first health check and the wait continues for
//     as long as that peer says it is working (waired-agent#1040,
//     peerwait.go). It writes nothing meanwhile, so when the watch does end
//     the turn the answer is still a status that can name the peer — which is
//     what waired-agent#1180 asked for.
//   - a LOCAL leg is never aborted, and writes an SSE keepalive so the wait
//     is not silent (docs/decisions/20260821/2142). This device loading its
//     own weights is not a failure, and there is nowhere else to send the
//     turn.
//
// Both used to be conditioned on X-Waired-Fallback-Allowed, which the
// intercept set on the auto leg alone: a leg with a fallback was aborted, a
// leg without one waited forever. The header is gone with the fallback, and
// the peer wait now covers every peer leg — including the pinned one, which
// used to hang silently
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md).
//
// The keepalive is LOCAL-only for a reason beyond symmetry: a peer leg's
// non-2xx can be the over-window 400 carrying
// HeaderLocalError=context_overflow, which relayPeerContextOverflow forwards
// as a STATUS and Claude Code keys auto-compaction off. Committing the stream
// early would turn that into an in-band error and the session could never
// compact. A local engine cannot produce that header — the over-window guard
// upstream in this handler runs pre-dispatch — so the restriction removes the
// hazard by construction rather than by care.
func waitPolicyFor(deps Deps, sel router.Selection, class string) waitPolicy {
	if strings.HasPrefix(sel.Runtime, remoteRuntimePrefix) {
		if deps.TTFBBudget == nil {
			return waitPolicy{}
		}
		wp := waitPolicy{Budget: deps.TTFBBudget(class), Reason: LocalErrorPeerTTFBTimeout}
		wp.Liveness = peerLivenessFor(deps, sel, class, wp.Budget)
		return wp
	}
	if deps.StreamKeepalive > 0 {
		return waitPolicy{Keepalive: deps.StreamKeepalive}
	}
	return waitPolicy{}
}

// peerLivenessFor plans the watch for a peer leg, or reports nil when this
// leg keeps the flat deadline (waired-agent#1040).
//
// Every condition is a way of saying "there is nothing to watch or nothing
// to extend":
//
//   - grace <= 0 — the operator turned the peer deadline off for this
//     class, and a watch would put a bound back where they removed one;
//   - no ceiling for this class, or one no longer than the grace — the
//     class is on the flat deadline by configuration (the subagent leg is
//     this case; see Deps.PeerWaitCeiling);
//   - nothing to ask with — neither the test seam nor a peer adapter
//     factory, so every check would fail as a transport error and read as
//     a peer that vanished;
//   - a selection that names no peer, which is not a peer leg at all.
func peerLivenessFor(deps Deps, sel router.Selection, class string, grace time.Duration) *peerLiveness {
	if grace <= 0 || deps.PeerWaitCeiling == nil {
		return nil
	}
	if deps.PeerHealth == nil && deps.PeerAdapterFactory == nil {
		return nil
	}
	ceiling := deps.PeerWaitCeiling(class)
	if ceiling <= grace {
		return nil
	}
	id, ok := peerDeviceIDOf(sel)
	if !ok {
		return nil
	}
	return &peerLiveness{PeerID: id, Grace: grace, Ceiling: ceiling}
}

// The ways a turn can come back with nothing the client can act on.
//
// engine_truncated_stream kept its name and narrowed to what the name
// says: bytes stopped arriving before the engine said it was done. The
// rest were carried by that same string until waired-agent#1179, which
// is how a reply that arrived whole and read fine came to be filed under
// truncation.
const (
	reasonEngineTruncatedStream = "engine_truncated_stream"
	reasonEngineThinkingOnly    = "engine_thinking_only"
	reasonEngineMarkupOnly      = "engine_markup_only"
	reasonEngineNoUsableTurn    = "engine_no_usable_turn"
)

// unusableTurnReason names which of those it was.
//
// The WARN at the end of proxyAnthropicStream has carried thinking_only
// and engine_markup_only as attributes since #786 — the distinction was
// already made, just not by the reason anyone reads back. Anyone grepping
// the event ring for truncation was being handed five other outcomes.
//
// Order is precedence. max_tokens stays first so the one outcome that is
// nobody's failure keeps reading that way, exactly as before; a cancelled
// context comes next because after it the remaining tests describe where
// the stream happened to be when we cut it, not what the engine did.
//
// Returns "" for the max_tokens case: not recorded as a failure.
func unusableTurnReason(ctx context.Context, usable, truncated bool, finishReason string, thinkingOpen, textOpen bool, watch *markupWatch) string {
	switch {
	case !usable && finishReason == "length":
		return ""
	case ctx != nil && ctx.Err() != nil:
		return LocalErrorClientDisconnected
	case truncated:
		return reasonEngineTruncatedStream
	case textOpen && watch.onlyEngineMarkup():
		return reasonEngineMarkupOnly
	case thinkingOpen && !textOpen:
		return reasonEngineThinkingOnly
	default:
		return reasonEngineNoUsableTurn
	}
}

// proxyAnthropicStream reads the engine's OpenAI SSE stream and
// rewrites it into Anthropic's event-typed SSE shape. Tool-call
// streaming is best-effort: deltas are buffered until finish_reason
// fires, then emitted as a single tool_use content_block (a known
// Phase A gap; spec gap recorded in docs/knowledges/20260502.md).
// rr may be nil (direct calls from tests). The stream loop already
// accumulates the upstream's usage object; metering reuses it, so a
// client that disconnects mid-stream still contributes whatever the
// engine reported before the break (waired#829).
func (h *HandlerSet) proxyAnthropicStream(ctx context.Context, client *http.Client, baseURL string, body []byte, originalModel string, offered []AnthropicTool, w http.ResponseWriter, wait waitPolicy, sel router.Selection, rr *requestRec, reporter runtime.FailureReporter) {
	// #757: bound only the PRE-first-byte window. reqCtx governs the peer
	// request; the watch below cancels it when the leg may not go on
	// waiting, so postToEngine errors BEFORE the stream commits and the
	// intercept's auto fallback reroutes. The watch is disarmed the instant
	// postToEngine returns (headers received), so a slow-but-progressing
	// completion is never cut mid-stream (mid-stream cancellation is #651).
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	preWatch := h.armPreCommitWatch(ctx, wait, cancel)

	// waired-agent#837: on a leg with nowhere else to send the turn, the wait
	// is legitimate but the silence is not — the client's own idle watchdog
	// closes a socket that has produced nothing, and its retry starts the
	// load again. Never armed together with the budget above; see
	// waitPolicyFor.
	start := time.Now()
	var hold *sseKeepalive
	if wait.Keepalive > 0 {
		hold = startSSEKeepalive(ctx, w, wait.Keepalive, func() {
			slog.Info("gateway: engine has produced no bytes yet; holding the stream open",
				append([]any{
					"model", recordedModel(rr),
					"engine_model", sel.EngineModel,
					"waited_ms", time.Since(start).Milliseconds(),
					"keepalive_ms", wait.Keepalive.Milliseconds(),
				}, h.localEngineLogFields(sel, rr)...)...)
		}, "model", recordedModel(rr))
	}
	resp, err := h.postToEngine(reqCtx, client, baseURL, "/v1/chat/completions", body)
	// Before ANY other write to w: stop returns only once no keepalive write
	// can still be in flight.
	hold.stop(holdStopReason(resp, err))
	abortReason, abortAfter := preWatch.disarm()
	if abortReason != "" {
		// The watch ended the leg (postToEngine may even have returned a
		// late success whose reqCtx we just cancelled). We are still
		// pre-commit, so stage the reason + elapsed wait, log, and answer
		// with a status rather than streaming a dead body. Nothing carries
		// the turn onwards any more, so the answer is the fail-closed 400
		// (see writeFailClosed).
		if resp != nil {
			_ = resp.Body.Close()
		}
		// Recorded, not just written: rr.succeed() runs before dispatch
		// (handleAnthropicMessagesImpl), so without this the event ring
		// keeps the pre-dispatch 200 and a leg that produced nothing
		// reads as a finished turn.
		rr.fail(http.StatusBadRequest, abortReason)
		w.Header().Set(HeaderLocalError, abortReason)
		who := "peer"
		if peer := w.Header().Get(HeaderInferencePeer); peer != "" {
			// waired-agent#1180: the turn ends here, so the message is what
			// the person reads. Naming the machine is the difference between
			// "something timed out" and "that computer stopped answering".
			who = "peer " + peer
		}
		if abortReason == LocalErrorEngineTTFBTimeout {
			// waired-agent#837: the same abort, about this device's own
			// engine. It is left part-way through a load nobody is waiting
			// on any more, so finish that load out of band — otherwise the
			// next turn pays for it again, which is the loop #837 reported.
			who = "this computer's engine"
			if h.deps.OnLocalEngineAbandoned != nil {
				h.deps.OnLocalEngineAbandoned()
			}
		}
		slog.Warn("gateway: the pre-first-byte wait ended without a first byte; failing the turn",
			append([]any{
				"leg", who,
				"peer", w.Header().Get(HeaderInferencePeer),
				"model", originalModel,
				"reason", abortReason,
				"waited_ms", abortAfter.Milliseconds(),
			}, h.localEngineLogFields(sel, rr)...)...)
		writeFailClosed(w, "waired_cannot_serve", preCommitAbortMessage(who, abortReason, abortAfter))
		return
	}
	if err != nil {
		// Same status and reason as the non-streaming twin, which has
		// recorded this since it was written: the two transports must not
		// describe one failure differently.
		reason := engineLegReason(ctx, "engine_request_failed")
		rr.fail(http.StatusBadGateway, reason)
		if !hold.committed() {
			w.Header().Set(HeaderLocalError, reason)
		}
		slog.Warn("gateway: the engine leg failed before any response headers",
			"reason", reason, "err", adapterErrorForClient(sel, err),
			"model", recordedModel(rr), "waited_ms", time.Since(start).Milliseconds())
		writeAnthropicErrorOrEvent(w, hold, http.StatusBadGateway, "upstream_error", adapterErrorForClient(sel, err))
		return
	}
	// A closure, not `defer resp.Body.Close()`: resp is reassigned when a
	// truncated stream is retried below, and the plain form would bind to
	// the first body and leak every later one.
	defer func() { _ = resp.Body.Close() }()
	slog.Debug("anthropic upstream stream", "status", resp.StatusCode, "ttfb_ms", time.Since(start).Milliseconds())
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		// Unreachable on a held stream by construction — a keepalive is
		// armed only for a LOCAL selection, and only a peer can send this
		// header — but the guard is kept so the relay can never write a
		// status onto a committed response if that ever stops being true.
		if !hold.committed() && relayPeerContextOverflow(w, resp, errBody, rr) {
			return
		}
		// Same as the non-stream leg: tell the adapter, and record the real
		// status (this leg recorded nothing at all before waired-agent#29).
		reportEngineFailure(reporter, resp.StatusCode, errBody)
		status, errType, reason := classifyEngineFailure(resp.StatusCode, errBody)
		if status != resp.StatusCode && !hold.committed() {
			slog.Warn("gateway: engine refused the request shape; surfacing 400 so the client stops retrying",
				"model", recordedModel(rr), "engine_status", resp.StatusCode)
			w.Header().Set(HeaderLocalError, LocalErrorEngineRequestShape)
		}
		rr.fail(status, reason)
		writeAnthropicErrorOrEvent(w, hold, status, errType, strings.TrimSpace(string(errBody)))
		return
	}

	if !hold.committed() {
		writeAnthropicStreamHeaders(w)
	}
	flusher, _ := w.(http.Flusher)

	emit := func(eventType string, payload any) {
		data, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, data)
		if flusher != nil {
			flusher.Flush()
		}
	}

	msgID := "msg_" + fmt.Sprintf("%d", time.Now().UnixNano())
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant",
			"model": originalModel, "content": []any{}, "stop_reason": nil,
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})

	// Block indices follow Anthropic convention thinking → text →
	// tool_use. Ollama streams the full reasoning trace before any
	// content, so thinking (when present) claims index 0 and text
	// shifts to 1. Tools are appended after both.
	thinkingOpen, thinkingClosed, textOpen := false, false, false
	textIdx := 0
	finishReason := ""
	usage := OpenAIUsage{}

	// Buffer for in-flight tool calls keyed by index. OpenAI streams
	// tool_calls as partial deltas with `arguments` concatenated; we
	// reassemble them and emit at finish time.
	tools := map[int]*partialTool{}
	toolOrder := []int{}
	nextToolKey := 0

	// #409: text deltas pass through the sieve, which releases prose
	// immediately and withholds only a tail that could be the start of a
	// tool call the engine failed to parse. Resolved at finish time,
	// below — there is no SSE event that un-sends a text_delta, so
	// anything emitted here is final.
	sieve := newToolTextSieve(offered)
	contentSeen := false

	// watch accumulates the assistant text actually emitted, so the
	// usable-turn verdict below can ask whether the whole turn was
	// leftover tool-call markup (waired-agent#786).
	watch := newMarkupWatch()

	// writeText emits assistant text the sieve has released, opening the
	// text block (and closing any thinking block) on first release
	// rather than on first delta — a delta that is entirely withheld
	// must not open a block the turn may never put anything in.
	writeText := func(s string) {
		if s == "" {
			return
		}
		watch.add(s)
		if thinkingOpen && !thinkingClosed {
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
			thinkingClosed = true
		}
		if !textOpen {
			if thinkingOpen {
				textIdx = 1
			}
			emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": textIdx,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			textOpen = true
		}
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": textIdx,
			"delta": map[string]any{"type": "text_delta", "text": s},
		})
	}

	// spent accumulates the usage of ABANDONED attempts. The wire keeps
	// reporting the surviving turn's own output_tokens, but metering has
	// to see the work a retry really cost or this failure mode looks free.
	var spent OpenAIUsage
	// Kept apart from spent because spent is a value-typed OpenAIUsage
	// whose nested details pointer cannot be summed into (waired-agent#885).
	var spentCached int

	// #442: an engine that fails mid-stream does not say so. Measured on
	// ollama 0.31.1 with qwen3.5:9b, 7 of 12 turns: no error frame, no
	// [DONE], finish_reason never set, and a body that closes cleanly. So
	// neither an `error` field on the chunk nor scanner.Err() can see it
	// — the only signature is the stream ending without the engine ever
	// saying HOW it ended.
	//
	// Retrying is worth it because each attempt is a fresh draw (~50% on
	// that model, so ~12% after two retries) and it needs no new parsing
	// of anything. It is only safe while nothing irrevocable has reached
	// the client: there is no SSE event that un-sends a text_delta or a
	// tool_use. Thinking does not count — it is the model's reasoning,
	// not its answer — but a retry's own reasoning is suppressed, so the
	// client is never shown two traces for one turn.
	truncated := false
	attempts := 0
	for {
		attempts++
		finishReason = ""
		sawDone := false
		scanner := bufio.NewScanner(resp.Body)
		// One SSE line is one frame, and a tool call's arguments can
		// arrive as a single input_json_delta — a Write of a large file
		// is one such frame. Over the limit the scanner stops with an
		// error, which reads here as a mid-stream truncation and is
		// reported to the client as the model failing (streamFailureNote)
		// after three attempts that fail identically. Matched to the
		// probe's reader (internal/agentgrade/stream.go), which already
		// carries the larger bound.
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			payload, ok := CutSSEData(line)
			if !ok {
				continue
			}
			if payload == "[DONE]" {
				sawDone = true
				break
			}
			var chunk struct {
				Choices []struct {
					Index int `json:"index"`
					Delta struct {
						Content          string `json:"content,omitempty"`
						Reasoning        string `json:"reasoning,omitempty"`
						ReasoningContent string `json:"reasoning_content,omitempty"`
						// Not []OpenAIToolCall: the STREAMING shape carries
						// an `index` the non-streaming one has no field
						// for, and it is the only reliable way to tell a
						// continuation of call N from the start of call
						// N+1 (see toolDeltaKey).
						ToolCalls []streamToolCallDelta `json:"tool_calls,omitempty"`
					} `json:"delta"`
					FinishReason string `json:"finish_reason,omitempty"`
				} `json:"choices"`
				Usage *OpenAIUsage `json:"usage,omitempty"`
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue
			}
			if chunk.Usage != nil {
				usage = *chunk.Usage
			}
			for _, ch := range chunk.Choices {
				// Reasoning arrives before content; stream it as a thinking
				// block at index 0. Ignore any stray reasoning once text has
				// started (thinking is closed at that point).
				reasoning := ch.Delta.Reasoning
				if reasoning == "" {
					reasoning = ch.Delta.ReasoningContent
				}
				// waired-agent#874: prefill ended the moment the engine
				// produced its first token, whatever kind it is. Tool
				// calls count — coding-agent turns are mostly tool calls,
				// and a content-only test would make the majority of the
				// measured population invisible. Reasoning counts because
				// it reaches the wire unsieved and is what a coding agent
				// renders as "Thought for N seconds": the user's first
				// visible output.
				//
				// Deliberately the ENGINE's token and not our first byte
				// out. The sieve below withholds text once a tool-call
				// sentinel appears, so stamping at writeText would report
				// our own buffering as prefill on exactly the turns that
				// matter. A bare role marker is not a token, so an empty
				// delta does not stamp.
				if reasoning != "" || ch.Delta.Content != "" || len(ch.Delta.ToolCalls) > 0 {
					rr.setFirstToken()
				}
				// attempts == 1 keeps a retry's reasoning off the wire. The
				// first attempt's trace is already streamed and cannot be
				// withdrawn, and one turn carrying two chains of thought
				// reads as a malfunction. What a retry is asked for is the
				// ANSWER; its reasoning is a second opinion nobody
				// requested. Worth saying out loud: the trace the client
				// keeps then belongs to the abandoned attempt. Same prompt
				// and same model make the two near-identical in practice,
				// and "near" is exactly why this is a comment and not a
				// silent condition.
				if reasoning != "" && !contentSeen && attempts == 1 {
					if !thinkingOpen {
						emit("content_block_start", map[string]any{
							"type": "content_block_start", "index": 0,
							"content_block": map[string]any{"type": "thinking", "thinking": ""},
						})
						thinkingOpen = true
					}
					emit("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": 0,
						"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
					})
				}
				if ch.Delta.Content != "" {
					contentSeen = true
					writeText(sieve.Push(ch.Delta.Content))
				}
				for _, tc := range ch.Delta.ToolCalls {
					key := toolDeltaKey(tc.Index, tc.ID, tools, toolOrder, &nextToolKey)
					p, ok := tools[key]
					if !ok {
						p = &partialTool{}
						tools[key] = p
						toolOrder = append(toolOrder, key)
					}
					if tc.ID != "" {
						p.ID = tc.ID
					}
					if tc.Function.Name != "" {
						p.Name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						p.Args.WriteString(tc.Function.Arguments)
					}
				}
				if ch.FinishReason != "" {
					finishReason = ch.FinishReason
				}
			}
		}

		// Whether the engine said HOW it ended is worth recording — a
		// stream that just stops is a different fault from one reporting
		// a clean finish — but it is NOT the retry condition. Measuring
		// found the same answerless turn arriving under both headings.
		truncated = scanner.Err() != nil || (!sawDone && finishReason == "")
		// Redrawable: this attempt produced nothing the client can act
		// on, and nothing irrevocable has been sent. max_tokens is
		// excluded — the model did produce, the budget ran out, and
		// re-drawing would only spend it again. A cancelled request is
		// excluded because there is nobody left to draw for: measured on
		// the rc5 host, the retry POST failed 0.5 ms later under the same
		// dead context, which is the "retried once" in
		// waired-agent#1179.
		redrawable := !contentSeen && len(toolOrder) == 0 && finishReason != "length" && ctx.Err() == nil
		if !redrawable || attempts > maxStreamRetries {
			break
		}
		slog.Warn("gateway: engine produced no usable turn; retrying",
			"model", recordedModel(rr), "attempt", attempts,
			"max_attempts", maxStreamRetries+1, "truncated", truncated,
			"finish_reason", finishReason, "scan_err", adapterErrorForClient(sel, scanner.Err()))
		next, nerr := h.postToEngine(reqCtx, client, baseURL, "/v1/chat/completions", body)
		if nerr != nil {
			slog.Warn("gateway: retrying a truncated stream failed", "err", adapterErrorForClient(sel, nerr))
			break
		}
		if next.StatusCode/100 != 2 {
			// The retry met a genuinely broken engine rather than another
			// bad draw. Tell the adapter and stop: further attempts would
			// measure the outage, not the model.
			errBody, _ := io.ReadAll(next.Body)
			_ = next.Body.Close()
			reportEngineFailure(reporter, next.StatusCode, errBody)
			break
		}
		// Close the abandoned attempt only now that it has a replacement,
		// so every body is closed exactly once (the last by the defer).
		_ = resp.Body.Close()
		spent.PromptTokens += usage.PromptTokens
		spent.CompletionTokens += usage.CompletionTokens
		// Summed like the two above, and for the same reason: the
		// abandoned attempt really did read that many cached tokens.
		// Because PromptTokens is summed too, cached <= input survives,
		// so their ratio stays readable as "the fraction of prompt
		// tokens the engine did not have to prefill" (waired-agent#885).
		spentCached += usage.CachedPromptTokens()
		usage = OpenAIUsage{}
		resp = next
	}

	// #409: settle the withheld tail. When the engine DID produce
	// structured tool_calls its parser plainly worked, so the text is
	// just text — release it untouched rather than hunting for a second
	// call in the model's own prose.
	var recovered recoveredCall
	recoveredOK := false
	if len(toolOrder) > 0 {
		writeText(sieve.Flush())
	} else {
		tail, c, ok := sieve.Finish()
		writeText(tail)
		recovered, recoveredOK = c, ok
	}
	if recoveredOK {
		rr.setToolRecovery(recovered.Shape)
		logToolRecovery(recovered.Shape, recovered.Name, originalModel, true)
	}

	if thinkingOpen && !thinkingClosed {
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	}
	if textOpen {
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": textIdx})
	}
	// Emit reassembled tool_use blocks after any thinking/text blocks.
	nextIdx := 0
	if thinkingOpen {
		nextIdx++
	}
	if textOpen {
		nextIdx++
	}
	// #442. What a client can act on is visible text, a tool call, or a
	// recovered one. Thinking is none of them: an agent handed a turn
	// that is only reasoning simply stalls, and keying this guard on
	// emptiness is exactly what let the defect through — the thinking
	// block is present, opened and closed correctly, ending on the model
	// deciding which tool to call, and it is all there is.
	//
	// A truncated turn is unusable even when some text did arrive: a
	// reply cut off mid-sentence and labelled end_turn is the same lie
	// in a longer coat. It gets its own block after whatever landed.
	//
	// stop_reason stays end_turn throughout. There is no Anthropic value
	// for "the engine gave up", and inventing one risks a client state
	// machine that has never seen it; the visible text carries the truth
	// instead.
	//
	// waired-agent#786: text alone was the whole test, and a turn whose
	// text is nothing but leftover tool-call markup passed it. Measured on
	// a mesh-served qwen3.5-2b under the Claude Code harness, the entire
	// reply was `<response>` / `</function>` / `</tool_call>` and the CLI
	// exited 0 with that on screen. Nothing here can un-send it — an
	// Anthropic SSE text_delta has no retraction (see the note on
	// toolTextSieve) — but calling it unusable adds the visible note below
	// and records the request as the failure it was, instead of metering
	// silent garbage as a served turn.
	//
	// Kept as separate conditions rather than one boolean so the next
	// dimension can be added to the verdict rather than replacing it.
	reportReasoningLeak(rr, watch.text())

	usable := len(toolOrder) > 0 || recoveredOK || (textOpen && !watch.onlyEngineMarkup())
	if !usable || (truncated && len(toolOrder) == 0 && !recoveredOK) {
		note := streamFailureNote(recordedModel(rr), attempts)
		reason := unusableTurnReason(ctx, usable, truncated, finishReason, thinkingOpen, textOpen, watch)
		switch reason {
		case "":
			// A different cause with a fix the reader can apply, and the
			// one case here that is nobody's failure to record: the
			// engine did exactly what the request asked for.
			note = truncationNote
		case LocalErrorClientDisconnected:
			// Nobody is reading this turn — the socket closed mid-stream
			// and that is why the reply stops where it does. A note here
			// would blame the model for our own hangup, in a transcript
			// no one will open (waired-agent#1179).
			note = ""
		case reasonEngineThinkingOnly:
			note = thinkingOnlyNote(recordedModel(rr), attempts)
		}
		if note != "" {
			emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": nextIdx,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": nextIdx,
				"delta": map[string]any{"type": "text_delta", "text": note},
			})
			emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": nextIdx})
			nextIdx++
		}
		if reason != "" {
			// Recorded as a failure, at the status the client actually
			// received: the 200 went out at the WriteHeader above, before
			// anything was known, and HTTP gives no way to take it back
			// (waired-agent#538). The reason is what makes it a failure —
			// RecordRequest labels on `Status >= 400 || ErrorReason != ""`,
			// so the error metric and the WARN below are unchanged by the
			// status, and a request metered as a success is still not one
			// nobody investigates.
			//
			// The status is also what decides whether the usage sample is
			// reported (emitUsage), and it must be: setUsage below folds
			// every abandoned attempt in on purpose, because the engine
			// really did that work (waired-agent#458). A 502 here threw
			// exactly those tokens away (waired-agent#554).
			rr.fail(http.StatusOK, reason)
			slog.Warn("gateway: no usable turn after every attempt",
				"model", recordedModel(rr), "attempts", attempts,
				"reason", reason,
				"truncated", truncated, "finish_reason", finishReason,
				"thinking_only", thinkingOpen && !textOpen,
				"engine_markup_only", textOpen && watch.onlyEngineMarkup())
		}
	}
	for i, k := range toolOrder {
		p := tools[k]
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": nextIdx,
			"content_block": map[string]any{
				"type": "tool_use", "id": toolUseID(p.ID, msgID, i),
				"name": p.Name, "input": map[string]any{},
			},
		})
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": nextIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": p.Args.String()},
		})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": nextIdx})
		nextIdx++
	}
	// The recovered call ships as an ordinary tool_use block, so the
	// client cannot tell it from one the engine parsed itself.
	stopReason := mapFinishReason(finishReason)
	if recoveredOK {
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": nextIdx,
			"content_block": map[string]any{
				"type": "tool_use", "id": recoveredToolUseID(msgID),
				"name": recovered.Name, "input": map[string]any{},
			},
		})
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": nextIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(recovered.Input)},
		})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": nextIdx})
		// nextIdx is deliberately not advanced: the recovered call is
		// the last block of the turn, and at most one is ever recovered.
		//
		// The engine saw no tool call, so it reported "stop". Left
		// alone, the client would end the turn instead of running the
		// tool it was just handed.
		stopReason = "tool_use"
	}
	// Meter from the accumulated usage object rather than the emitted
	// map: the SSE shape the client sees is unchanged by this PR.
	// Metered with the abandoned attempts folded in: the engine really
	// did that work, and leaving it out would make a model that needs
	// three tries look as cheap as one that needs none.
	rr.setUsage(int64(spent.PromptTokens+usage.PromptTokens),
		int64(spent.CompletionTokens+usage.CompletionTokens))
	rr.setCachedInput(int64(spentCached + usage.CachedPromptTokens()))
	emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]int{"output_tokens": usage.CompletionTokens},
	})
	emit("message_stop", map[string]any{"type": "message_stop"})
}

func (h *HandlerSet) postToEngine(ctx context.Context, client *http.Client, baseURL, path string, body []byte) (*http.Response, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	u.Path = singleSlash(u.Path, path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if client == nil {
		client = h.deps.HTTPClient
	}
	return client.Do(req)
}

// respondAnthropicSelectionError renders a router selection error in
// Anthropic's envelope. queuedFor is how long selectAndProbe already
// held the request waiting for an admission slot (0 when it did not
// wait); it sizes the capacity Retry-After — see retryAfterForCapacity.
// failClosedExits is the tail every fail-closed message carries: what the
// reader can do about it. A turn addressed to Waired that Waired cannot answer
// now ends there — nothing carries it to the real Anthropic API on waired's
// own judgement — so the message has to hand the reader both ways out
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
// decision 4, owner ruling waired-ai/waired#1313).
const failClosedExits = " Pick an Anthropic model in /model to send this turn to the cloud, or run `waired doctor` to see what is missing."

// writeFailClosed answers a turn Waired cannot serve, in the one shape Claude
// Code shows at once and verbatim.
//
// The status is 400. Claude Code retries 5xx, 529 and 429 up to ten times
// before showing anything, rewrites a 401/403 as an authentication failure,
// and replaces a 404's message with its own "the selected model may not
// exist" (https://code.claude.com/docs/en/errors#automatic-retries; measured
// against Claude Code 2.1.259 on 2026-09-03 — 400 arrived once, with the
// message intact). None of the reasons routed here clears by itself, so the
// ten retries the old 503 bought were a minute of an anonymous "API error"
// and no information (waired-agent#1180). Product contract, ratified by
// docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md
// decision 4.
//
// A condition that DOES clear on its own keeps its retryable shape: weights
// still arriving, every peer momentarily at capacity, a runtime still being
// installed. Those stay 503 with a Retry-After, and anthropicSelectionStatus
// is where the two sets are decided — once, so the event ring records the
// status the client actually received (waired-agent#740).
func writeFailClosed(w http.ResponseWriter, errType, detail string) {
	writeAnthropicError(w, http.StatusBadRequest, errType, failClosedMessage(detail))
}

// failClosedMessage is the sentence the person reads.
//
// The internal string is a diagnosis addressed to whoever is reading logs —
// "router: model is not in ready state on disk" — and it arrives in Claude
// Code as the whole explanation of why their turn stopped. So the package
// prefix goes, the sentence is capitalised and stopped, and the two ways out
// follow it.
func failClosedMessage(detail string) string {
	detail = strings.TrimSpace(detail)
	for _, prefix := range []string{"router: ", "gateway: "} {
		detail = strings.TrimPrefix(detail, prefix)
	}
	if detail == "" {
		detail = "Waired could not answer this turn"
	}
	r := []rune(detail)
	detail = string(unicode.ToUpper(r[0])) + string(r[1:])
	if !strings.HasSuffix(detail, ".") {
		detail += "."
	}
	return detail + failClosedExits
}

// modelTooSmallDetail words the operator's routing floor for them. It names
// the setting they can change rather than a machine that might be broken
// (waired-agent#1128).
//
// The phrasing rule is an owner ruling (2026-08-29, waired-agent#1128), pinned
// as an example in docs-site/TRANSLATION.md: "large" has no larger class above
// it, so "a large model or larger" would be a sentence about a ladder that
// ends there. It used to live in the Claude intercept's reroute notice, which
// went with the reroute
// (docs/decisions/20260903/0333-no-automatic-crossing-to-or-from-anthropic.md);
// the router's own error text still carries the forbidden form, and
// waired-agent#1178 is moving the rule there — at which point this can defer
// to it.
func modelTooSmallDetail(floor string) string {
	if floor == "" {
		return "No computer on Waired runs a model large enough for your routing floor. " +
			"Change it with `waired worker set --min-model-size`"
	}
	phrase := "a " + floor + " model or larger"
	if floor == "large" {
		phrase = "a large model"
	}
	return "No computer on Waired runs " + phrase +
		". Change the floor with `waired worker set --min-model-size`"
}

// anthropicSelectionStatus is the status a selection error produces on the
// Claude surface, for BOTH the response and the event ring — the two must
// agree or a reader comparing them sees a disagreement the gateway invented
// (waired-agent#740).
//
// The split is terminal vs transient, not severity: 400 for a turn nothing
// will answer without someone acting, 503 with a Retry-After for one that a
// wait really does resolve.
func anthropicSelectionStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, router.ErrModelNotReady) && router.ModelIsArriving(err):
		// Weights are queued, downloading or being verified.
		return http.StatusServiceUnavailable
	case errors.Is(err, router.ErrAllPeersOverloaded),
		errors.Is(err, router.ErrPeersDidNotAnswer),
		errors.Is(err, router.ErrRuntimeNotInstalled):
		return http.StatusServiceUnavailable
	case router.BelowModelSizeFloor(err),
		errors.Is(err, router.ErrModelNotFound),
		errors.Is(err, router.ErrCapabilityNotMet),
		errors.Is(err, router.ErrLocalInferenceOff),
		errors.Is(err, router.ErrModelNotReady),
		errors.Is(err, router.ErrPinnedPeerUnreachable),
		errors.Is(err, router.ErrHardwareInsufficient),
		errors.Is(err, ErrPeerRoutingDisabled):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func respondAnthropicSelectionError(w http.ResponseWriter, err error, queuedFor time.Duration) {
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
		floor := router.ModelSizeFloor(err)
		if floor != "" {
			w.Header().Set(HeaderMinModelSize, floor)
		}
		writeFailClosed(w, "waired_model_too_small", modelTooSmallDetail(floor))
	case errors.Is(err, router.ErrModelNotFound):
		writeFailClosed(w, "not_found_error", err.Error())
	case errors.Is(err, router.ErrCapabilityNotMet):
		writeFailClosed(w, "invalid_request_error", err.Error())
	case errors.Is(err, router.ErrLocalInferenceOff):
		// The header lets the surfaces name the toggle rather than a bare
		// status (waired-agent#829).
		w.Header().Set(HeaderLocalError, LocalErrorInferenceDisabled)
		writeFailClosed(w, "waired_inference_disabled", err.Error())
	case errors.Is(err, router.ErrModelNotReady):
		if router.ModelIsArriving(err) {
			// Weights are queued, downloading or being verified: waiting
			// really does end, so keep the retryable shape.
			w.Header().Set("Retry-After", "30")
			writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", err.Error())
			return
		}
		// waired-agent#788: no host serves this model and none is fetching
		// it. Nothing to wait for.
		w.Header().Set(HeaderLocalError, LocalErrorModelNotServed)
		writeFailClosed(w, "not_found_error", err.Error())
	case errors.Is(err, router.ErrAllPeersOverloaded):
		// Phase 7: every matching mesh peer was at its concurrent-
		// request cap. Anthropic API uses "overloaded_error" for the
		// equivalent state — keep the wire shape stable. Retryable on
		// purpose: a slot frees up.
		w.Header().Set("Retry-After", retryAfterForCapacity(queuedFor))
		writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", err.Error())
	case errors.Is(err, router.ErrPeersDidNotAnswer):
		// Matching peers existed but none answered its readiness probe.
		// Anthropic's envelope has no code for "unmeasured", so the wire
		// shape stays overloaded_error; the message carries the
		// distinction, and the observability reason
		// (selectionFailureReason) records it exactly.
		w.Header().Set("Retry-After", "5")
		writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", err.Error())
	case errors.Is(err, ErrPeerRoutingDisabled):
		// Phase 8: probe path bubbled up a uniform routing-disabled
		// signal. A wiring fault, not a wait.
		writeFailClosed(w, "runtime_unavailable", err.Error())
	case errors.Is(err, router.ErrPinnedPeerUnreachable):
		// An operator-pinned peer is absent / stale / disco-unreachable.
		// It clears when the peer returns, but nothing here will bring it
		// back, and a pin is not substituted (waired-agent#325) — so the
		// turn ends now, naming the peer, rather than after ten anonymous
		// retries (waired-agent#1180).
		w.Header().Set(HeaderLocalError, LocalErrorPinnedPeerUnreachable)
		if peer := pinnedPeerOf(err); peer != "" {
			w.Header().Set(HeaderInferencePeer, peer)
		}
		writeFailClosed(w, "waired_pinned_peer_unreachable", err.Error())
	case errors.Is(err, router.ErrHardwareInsufficient):
		writeFailClosed(w, "invalid_request_error", err.Error())
	case errors.Is(err, router.ErrRuntimeNotInstalled):
		writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", err.Error())
	default:
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
	}
}

// retryAfterForCapacity sizes the Retry-After for a request answered
// "every matching mesh peer is at capacity" (waired-agent#786).
//
// The historical value is five seconds, and it stays the floor. What it
// could not express is a request the gateway already queued: when
// selectAndProbe held the caller for twenty seconds and the slot never
// freed, the peer is busy with something longer than that, and sending
// the caller back in five only buys another rejection. So the hint is at
// least as long as the wait that just failed.
func retryAfterForCapacity(queuedFor time.Duration) string {
	const floor = 5 * time.Second
	if queuedFor <= floor {
		return "5"
	}
	secs := int((queuedFor + time.Second - 1) / time.Second) // round up
	return strconv.Itoa(secs)
}

// hasMetadataFeature returns true if the metadata field is a JSON
// object that carries a non-zero entry under key. Phase A uses this
// to reject the few metadata-driven features (cache_control, beta
// gates) that the engine cannot honour.
func hasMetadataFeature(raw json.RawMessage, key string) bool {
	if len(raw) == 0 {
		return false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	v, ok := m[key]
	return ok && len(v) > 0 && string(v) != "null"
}

// relayPeerContextOverflow turns a serving peer's over-window refusal
// into the Anthropic 400 the local client compacts on, and reports
// whether it did.
//
// The peer answers OpenAI-shaped, so its 400 would otherwise arrive here
// as an "upstream_error" — a fault, which Claude Code retries or
// surfaces rather than compacting. HeaderLocalError is how the peer says
// which kind of 400 it sent, and re-emitting the canonical envelope is
// what makes a mesh leg behave like a local one (waired-agent#436).
//
// The staged header also marks it "surface, don't fall back" for the
// intercept's auto mode: falling back to the real Anthropic API here
// would abandon local serving for a turn that only needed compacting.
func relayPeerContextOverflow(w http.ResponseWriter, resp *http.Response, body []byte, rr *requestRec) bool {
	if resp.StatusCode != http.StatusBadRequest ||
		resp.Header.Get(HeaderLocalError) != LocalErrorContextOverflow {
		return false
	}
	rr.fail(http.StatusBadRequest, "context_overflow")
	slog.Debug("peer refused an over-window prompt", "detail", strings.TrimSpace(string(body)))
	w.Header().Set(HeaderLocalError, LocalErrorContextOverflow)
	writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
		peerContextOverflowMessage(body))
	return true
}

// peerContextOverflowMessage keeps the peer's own numbers when it sent
// them in the shape this gateway writes, and falls back to a generic
// line otherwise. Claude Code keys compaction off the status and the
// error type, not this string, so an unparseable body is not a reason to
// drop the signal.
func peerContextOverflowMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && strings.HasPrefix(e.Error.Message, "prompt is too long") {
		return e.Error.Message
	}
	return "prompt is too long for the serving engine's context window"
}

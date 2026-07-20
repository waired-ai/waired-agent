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
	"strings"
	"sync"
	"time"

	"github.com/waired-ai/waired-agent/internal/router"
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

// handleAnthropicMessages overrides the stub in server.go. It accepts
// an Anthropic Messages request, translates to OpenAI Chat Completions
// (preserving Anthropic semantics where they diverge — see
// docs/knowledges/20260502.md), proxies to the selected engine, then
// translates the response (or stream) back to Anthropic's wire shape.
func (h *HandlerSet) handleAnthropicMessagesImpl(w http.ResponseWriter, r *http.Request) {
	rr := h.startRequest("anthropic")
	defer rr.finish()

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
	stickyID := ComputeStickyID(r.Header, body)
	if stickyID != "" && class != "" {
		stickyID += ":" + class
	}
	routeReq := router.Request{Model: req.Model, StickyID: stickyID, Class: class}
	probed, err := h.selectAndProbe(r.Context(), routeReq)
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
		probed, err = h.selectAndProbe(r.Context(), routeReq)
		if err == nil {
			w.Header().Set(HeaderLocalModel, mapped)
		}
	}
	if err != nil {
		rr.ev.Model = routeReq.Model // the mapped id when mapping was applied
		rr.fail(selectionStatus(err), selectionErrorReason(err))
		respondAnthropicSelectionError(w, err)
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
	openaiReq.Model = sel.EngineModel

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
	if h.deps.ContextWindowFor != nil {
		if win := h.deps.ContextWindowFor(sel.ModelID); win > 0 {
			if n := CountTokensApprox(req); n > win {
				rr.fail(http.StatusBadRequest, "context_overflow")
				w.Header().Set(HeaderLocalError, LocalErrorContextOverflow)
				writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
					fmt.Sprintf("prompt is too long: %d tokens > %d maximum", n, win))
				return
			}
		}
	}

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
		writeAnthropicError(w, http.StatusServiceUnavailable, "runtime_unhealthy", err.Error())
		return
	}

	encoded, err := json.Marshal(openaiReq)
	if err != nil {
		rr.fail(http.StatusInternalServerError, "encode_failed")
		writeAnthropicError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	rr.succeed()
	client := h.clientFor(adapter)
	if req.Stream {
		h.proxyAnthropicStream(r.Context(), client, adapter.BaseURL(), encoded, req.Model, w,
			ttfbBudgetFor(h.deps, sel, r, class))
		return
	}
	h.proxyAnthropicNonStream(r.Context(), client, adapter.BaseURL(), encoded, req.Model, w)
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

func (h *HandlerSet) proxyAnthropicNonStream(ctx context.Context, client *http.Client, baseURL string, body []byte, originalModel string, w http.ResponseWriter) {
	resp, err := h.postToEngine(ctx, client, baseURL, "/v1/chat/completions", body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	if resp.StatusCode/100 != 2 {
		// Pass through upstream's error verbatim, wrapping it in our
		// envelope so clients still see Anthropic-shaped errors.
		writeAnthropicError(w, resp.StatusCode, "upstream_error", strings.TrimSpace(string(respBody)))
		return
	}
	var openaiResp OpenAIResponse
	if err := json.Unmarshal(respBody, &openaiResp); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "upstream_error", "malformed engine response: "+err.Error())
		return
	}
	out := OpenAIToAnthropic(openaiResp, originalModel)
	writeJSON(w, http.StatusOK, out)
}

// ttfbBudgetFor returns the pre-commit time-to-first-byte deadline to arm for
// this streaming leg, or 0 to disable it (#757). It is armed only for a PEER
// leg (remote:*) that the intercept authorized for fallback via the
// X-Waired-Fallback-Allowed request header (auto mode) — so a locally-served
// turn, or a peer leg under a pinned/waired-only route, is never aborted.
func ttfbBudgetFor(deps Deps, sel router.Selection, r *http.Request, class string) time.Duration {
	if deps.TTFBBudget == nil ||
		!strings.HasPrefix(sel.Runtime, remoteRuntimePrefix) ||
		r.Header.Get(HeaderFallbackAllowed) != "1" {
		return 0
	}
	return deps.TTFBBudget(class)
}

// proxyAnthropicStream reads the engine's OpenAI SSE stream and
// rewrites it into Anthropic's event-typed SSE shape. Tool-call
// streaming is best-effort: deltas are buffered until finish_reason
// fires, then emitted as a single tool_use content_block (a known
// Phase A gap; spec gap recorded in docs/knowledges/20260502.md).
func (h *HandlerSet) proxyAnthropicStream(ctx context.Context, client *http.Client, baseURL string, body []byte, originalModel string, w http.ResponseWriter, ttfb time.Duration) {
	// #757: bound only the PRE-first-byte window. reqCtx governs the peer
	// request; a time.AfterFunc cancels it if the engine returns no headers
	// within ttfb, so postToEngine errors BEFORE the stream commits and the
	// intercept's auto fallback reroutes. The timer is disarmed the instant
	// postToEngine returns (headers received), so a slow-but-progressing
	// completion is never cut mid-stream (mid-stream cancellation is #651).
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		ttfbMu       sync.Mutex
		ttfbFired    bool
		ttfbDisarmed bool
		ttfbTimer    *time.Timer
	)
	if ttfb > 0 {
		ttfbTimer = time.AfterFunc(ttfb, func() {
			ttfbMu.Lock()
			defer ttfbMu.Unlock()
			if ttfbDisarmed {
				return
			}
			ttfbFired = true
			cancel()
		})
	}

	resp, err := h.postToEngine(reqCtx, client, baseURL, "/v1/chat/completions", body)
	var firedBeforeHeaders bool
	if ttfbTimer != nil {
		ttfbMu.Lock()
		ttfbDisarmed = true
		firedBeforeHeaders = ttfbFired
		ttfbMu.Unlock()
		ttfbTimer.Stop()
	}
	if firedBeforeHeaders {
		// The deadline fired (postToEngine may even have returned a late
		// success whose reqCtx we just cancelled). We are still pre-commit,
		// so stage the reason + budget, log, and 502 so the intercept's
		// auto mode falls back instead of streaming a dead body.
		if resp != nil {
			_ = resp.Body.Close()
		}
		w.Header().Set(HeaderLocalError, LocalErrorPeerTTFBTimeout)
		w.Header().Set(HeaderTTFBBudgetMs, fmt.Sprintf("%d", ttfb.Milliseconds()))
		slog.Warn("gateway: peer produced no first byte within TTFB budget; failing pre-commit for fallback",
			"peer", w.Header().Get(HeaderInferencePeer),
			"model", originalModel,
			"budget_ms", ttfb.Milliseconds(),
		)
		writeAnthropicError(w, http.StatusBadGateway, "upstream_error",
			fmt.Sprintf("peer produced no response within %s", ttfb))
		return
	}
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		writeAnthropicError(w, resp.StatusCode, "upstream_error", strings.TrimSpace(string(errBody)))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
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
	type partialTool struct {
		ID, Name string
		Args     bytes.Buffer
	}
	tools := map[int]*partialTool{}
	toolOrder := []int{}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		var chunk struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content          string           `json:"content,omitempty"`
					Reasoning        string           `json:"reasoning,omitempty"`
					ReasoningContent string           `json:"reasoning_content,omitempty"`
					ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
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
			if reasoning != "" && !textOpen {
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
					"delta": map[string]any{"type": "text_delta", "text": ch.Delta.Content},
				})
			}
			for _, tc := range ch.Delta.ToolCalls {
				idx := tc.Function.Name // index by name when OpenAI doesn't echo index field; fallback to ID
				_ = idx
				key := len(tools)
				if existing, ok := tools[key]; ok && tc.Function.Name != "" && tc.Function.Name != existing.Name {
					key = len(tools) // start a new partial
				}
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
	// Safety net: a max_tokens truncation that produced no thinking,
	// text, or tool block would otherwise stream an empty turn and stall
	// the agentic loop. Emit one visible note so the client always gets
	// an actionable turn (mirrors OpenAIToAnthropic's non-stream guard).
	if !thinkingOpen && !textOpen && len(toolOrder) == 0 && finishReason == "length" {
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": truncationNote},
		})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		nextIdx = 1
	}
	for _, k := range toolOrder {
		p := tools[k]
		emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": nextIdx,
			"content_block": map[string]any{"type": "tool_use", "id": p.ID, "name": p.Name, "input": map[string]any{}},
		})
		emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": nextIdx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": p.Args.String()},
		})
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": nextIdx})
		nextIdx++
	}
	emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": mapFinishReason(finishReason), "stop_sequence": nil},
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

func respondAnthropicSelectionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, router.ErrModelNotFound):
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", err.Error())
	case errors.Is(err, router.ErrCapabilityNotMet):
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
	case errors.Is(err, router.ErrModelNotReady):
		w.Header().Set("Retry-After", "30")
		writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", err.Error())
	case errors.Is(err, router.ErrAllPeersOverloaded):
		// Phase 7: every matching mesh peer was at its concurrent-
		// request cap. Anthropic API uses "overloaded_error" for the
		// equivalent state — keep the wire shape stable.
		w.Header().Set("Retry-After", "5")
		writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", err.Error())
	case errors.Is(err, ErrPeerRoutingDisabled):
		// Phase 8: probe path bubbled up a uniform routing-disabled
		// signal. Same shape as the existing runtime_unavailable
		// error (line 102) so the wire envelope stays consistent
		// between the post-Selector lookup path and the Phase 8
		// pre-Selector probe path.
		writeAnthropicError(w, http.StatusServiceUnavailable, "runtime_unavailable", err.Error())
	case errors.Is(err, router.ErrPinnedPeerUnreachable):
		// An operator-pinned peer is absent / stale / disco-unreachable.
		// 503 (not the historical default:'s 500 api_error) because the
		// condition is environmental, not a gateway bug, and clears when
		// the peer returns. The staged HeaderLocalError turns the
		// intercept's fallback reason into local_pinned_peer_unreachable
		// so the operator sees *why* Claude traffic left the pin.
		w.Header().Set(HeaderLocalError, "pinned_peer_unreachable")
		w.Header().Set("Retry-After", "5")
		writeAnthropicError(w, http.StatusServiceUnavailable, "waired_pinned_peer_unreachable", err.Error())
	case errors.Is(err, router.ErrHardwareInsufficient):
		writeAnthropicError(w, http.StatusUnprocessableEntity, "invalid_request_error", err.Error())
	case errors.Is(err, router.ErrRuntimeNotInstalled):
		writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error", err.Error())
	default:
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
	}
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

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// keepaliveFrame is what holds a streaming response open while the engine
// has produced nothing.
//
// It is an SSE COMMENT line, not an Anthropic event. A comment is the SSE
// spec's own keepalive construct: every conformant decoder drops it before
// event assembly, so it cannot perturb Anthropic's message_start-first event
// order, it renders nothing to the reader, and the repo's own SSE readers
// (internal/agentgrade readAnthropicStream, and the translation loop below)
// skip it because it carries no "data: " prefix.
//
// A `ping` event was the other candidate and was rejected: whether a ping
// before message_start is accepted is a property of someone else's client,
// and a comment needs no such promise.
const keepaliveFrame = ": waired keepalive\n\n"

// jsonPadFrame is what holds a NON-STREAMING response open while the engine
// has produced nothing (waired-agent#1314).
//
// One space. JSON's grammar allows insignificant whitespace before a value
// (RFC 8259 §2), so a client that reads the whole body and parses it gets the
// same value it would have got from the unpadded body — measured on the
// shipping consumer, which parsed a body carrying 79 of these.
//
// An SSE comment cannot be used here and neither can any other frame with
// content: this leg promised the client ONE JSON object, and Claude Code
// ends the turn with "the non-streaming request was answered with a stream"
// the moment it is handed anything else.
const jsonPadFrame = " "

// holdShape is what one hold writes, and what writing it costs.
//
// Both members are per-leg because the three legs that arm a hold speak
// different dialects — two SSE ones that differ only in their headers, and
// the non-streaming one whose frames are not events at all.
type holdShape struct {
	// commit writes the response headers that make w the kind of response
	// this shape can write frames into, on the first frame and never
	// again. Every shape has one: HTTP has no way to send a byte of a
	// response without choosing its status, which is the whole cost of
	// speaking early.
	commit func(http.ResponseWriter)
	// frame writes one frame and reports whether the client is still
	// there.
	frame func(http.ResponseWriter) error
	// name is what the closing log line calls this shape.
	name string
	// inBand reports whether a failure can still be described to the
	// client once this shape has committed. True for the SSE shapes,
	// which have a frame for it (`event: error`, `data: {"error":…}`);
	// false for the non-streaming one, whose whole response is a single
	// object with no member to put a failure in.
	inBand bool
}

// sseCommentFrame writes the keepalive comment line. The frame half of both
// streaming shapes: a comment is a comment in either dialect.
//
// Both SSE shapes carry inBand: see writeAnthropicErrorOrEvent and
// writeOpenAIErrorFrame.
func sseCommentFrame(w http.ResponseWriter) error {
	_, err := io.WriteString(w, keepaliveFrame)
	return err
}

// jsonPadWhitespace writes one insignificant space into a JSON body.
func jsonPadWhitespace(w http.ResponseWriter) error {
	_, err := io.WriteString(w, jsonPadFrame)
	return err
}

// writeJSONBodyHeaders commits a non-streaming JSON response. No
// Content-Length: the length is not known until the engine answers, and
// omitting it is what puts the response on chunked encoding so each pad
// frame reaches the client as it is written.
func writeJSONBodyHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// engineHold holds a response open while the engine has produced nothing at
// all (waired-agent#837, waired-agent#1314).
//
// Lifetime is declared rather than implicit: it starts on the first FRAME —
// never at t=0 — and ends at stop(reason), and both ends are logged. A turn
// whose engine answers before that first frame therefore writes nothing and
// is byte-identical to the same turn before this existed.
//
// It is never capped. This runs only on a leg with nowhere else to send the
// turn, and a response that spoke and then went silent again is worse than
// one that never spoke.
//
// Concurrency contract: between start and stop, the hold OWNS w. stop takes
// the same mutex the frames are written under and latches, so no write is in
// flight once it returns — only then may the caller write the response.
type engineHold struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	stopped bool
	frames  int
	started time.Time
	// shape is what this hold writes and what writing it commits; see
	// holdShape.
	shape     holdShape
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
	onCommit  func()
	logFields []any
}

// startSSEKeepalive arms an SSE-comment hold on w and returns it. A nil
// return is never produced: callers hold the value and call stop
// unconditionally, and every method is nil-safe so a leg that armed nothing
// can share one code path with a leg that did.
//
// onCommit runs once, under the lock, immediately before the first frame
// reaches the wire — the moment the response status stops being ours to
// choose.
func startSSEKeepalive(ctx context.Context, w http.ResponseWriter, every time.Duration, commit func(http.ResponseWriter), onCommit func(), logFields ...any) *engineHold {
	if commit == nil {
		commit = writeAnthropicStreamHeaders
	}
	return startEngineHold(ctx, w, every, every,
		holdShape{commit: commit, frame: sseCommentFrame, name: "sse", inBand: true}, onCommit, logFields...)
}

// startJSONPadHold arms the non-streaming hold: silent for first, then one
// insignificant space every every (waired-agent#1314).
//
// first is not every, and the gap is the whole design. On a stream the first
// frame is cheap, so it goes out after one interval and the wait is visible
// from the start. Here the first frame SPENDS THE STATUS on a leg that has
// no in-band way to report a failure afterwards — measured, a client handed
// an error envelope under a committed 200 tells the reader to suspect their
// proxy. So the silence is kept for as long as it is worth more than the
// bytes, and given up only once the alternative is the client's own
// deadline.
func startJSONPadHold(ctx context.Context, w http.ResponseWriter, first, every time.Duration, onCommit func(), logFields ...any) *engineHold {
	return startEngineHold(ctx, w, first, every,
		holdShape{commit: writeJSONBodyHeaders, frame: jsonPadWhitespace, name: "json-pad"}, onCommit, logFields...)
}

func startEngineHold(ctx context.Context, w http.ResponseWriter, first, every time.Duration, shape holdShape, onCommit func(), logFields ...any) *engineHold {
	if first <= 0 || every <= 0 || shape.commit == nil || shape.frame == nil {
		return nil
	}
	kctx, cancel := context.WithCancel(ctx)
	k := &engineHold{
		w:         w,
		stopped:   false,
		started:   time.Now(),
		shape:     shape,
		cancel:    cancel,
		done:      make(chan struct{}),
		onCommit:  onCommit,
		logFields: logFields,
	}
	k.flusher, _ = w.(http.Flusher)
	go k.run(kctx, first, every)
	return k
}

// run waits first for the opening frame and every for each one after. A
// timer rather than a ticker because those two are different durations; the
// cadence after a frame is therefore every-plus-the-write rather than a
// fixed phase, which no keepalive has ever needed.
func (k *engineHold) run(ctx context.Context, first, every time.Duration) {
	defer close(k.done)
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !k.tick() {
				return
			}
			t.Reset(every)
		}
	}
}

// tick writes one frame. It reports false once the hold is finished,
// either because stop ran or because the client is gone.
func (k *engineHold) tick() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return false
	}
	if k.frames == 0 {
		// The response status stops being ours here. The same headers the
		// leg would have written on its normal path, hoisted so the held
		// and unheld forms of one leg cannot drift apart.
		k.shape.commit(k.w)
		if k.onCommit != nil {
			k.onCommit()
		}
	}
	if err := k.shape.frame(k.w); err != nil {
		// A write error here is the client hanging up. Latch rather than
		// retry: the stream is gone and the request context is about to
		// say so anyway.
		k.stopped = true
		return false
	}
	if k.flusher != nil {
		k.flusher.Flush()
	}
	k.frames++
	return true
}

// committed reports whether any frame reached the wire — i.e. whether the
// response status has already been spent. Nil-safe.
func (k *engineHold) committed() bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.frames > 0
}

// canReportInBand reports whether a failure can still be described to the
// client: either the status is unspent, or the shape that spent it has a
// frame for a failure. False means the only honest end is abortHeldResponse.
// Nil-safe.
func (k *engineHold) canReportInBand() bool {
	if !k.committed() {
		return true
	}
	return k.shape.inBand
}

// abortHeldResponse ends a response whose status is spent and whose shape
// cannot describe a failure. It closes the connection without the
// terminating chunk; http.ErrAbortHandler is the server's own way to say
// that, and it is logged rather than stack-traced.
//
// Measured against Claude Code 2.1.269: the reader is told "Connection to the
// API was lost (ECONNRESET). This is usually temporary — try again." — which
// is what happened and what to do. The alternative, a JSON error envelope
// under the committed 200, is read as "API returned an empty or malformed
// response (HTTP 200) — check for a proxy or gateway intercepting the
// request", which sends them after their own network for this computer's
// engine (docs/knowledges/20260912/2130-nonstream-leg-held-only-by-committing.md).
func abortHeldResponse(fields ...any) {
	slog.Warn("gateway: the leg failed after the hold had committed and the shape has no way to say so; closing the response", fields...)
	panic(http.ErrAbortHandler)
}

// stop ends the hold and returns only once no write can still be in flight,
// so the caller may write the response itself. Idempotent and nil-safe.
func (k *engineHold) stop(reason string) {
	if k == nil {
		return
	}
	k.stopOnce.Do(func() {
		k.mu.Lock()
		k.stopped = true
		frames := k.frames
		k.mu.Unlock()
		k.cancel()
		<-k.done
		if frames == 0 {
			// Nothing was written, so there is nothing to declare the end
			// of: the engine answered inside one interval and this leg is
			// indistinguishable from one that never armed a keepalive.
			return
		}
		slog.Info("gateway: stream hold ended",
			append([]any{
				"reason", reason,
				"shape", k.shape.name,
				"waited_ms", time.Since(k.started).Milliseconds(),
				"frames", frames,
			}, k.logFields...)...)
	})
}

// engineLegFailureMsg says where a failed engine leg landed relative to the
// response status, which is not a fixed fact about the leg: a wait long enough
// to commit the hold puts the same failure on the far side of it. Both legs
// read it from here so one cannot go on claiming "before any headers" while
// the other stops (waired-agent#1314).
func engineLegFailureMsg(hold *engineHold) string {
	if hold.committed() {
		return "gateway: the engine leg failed after the hold had committed the response"
	}
	return "gateway: the engine leg failed before any response headers"
}

// holdStopReason names why the keepalive ended, for the closing log line.
// It reads the same two values the caller is about to branch on, so the log
// and the branch can never disagree.
func holdStopReason(resp *http.Response, err error) string {
	switch {
	case err != nil:
		return "engine_request_failed"
	case resp == nil:
		return "no_response"
	case resp.StatusCode/100 != 2:
		return "engine_error"
	default:
		return "first_byte"
	}
}

// writeAnthropicErrorOrEvent renders a failure to a client that may already
// be reading a committed stream (waired-agent#837).
//
// Pre-commit it is writeAnthropicError, byte for byte. Post-commit the status
// is spent and HTTP gives no way to take it back, so the same envelope goes
// out as one SSE `error` event — the shape Anthropic's own stream uses for a
// mid-stream failure — and the caller's rr.fail records the real status
// regardless, which is what keeps the event ring honest (waired-agent#538).
func writeAnthropicErrorOrEvent(w http.ResponseWriter, hold *engineHold, status int, errType, message string) {
	if !hold.committed() {
		writeAnthropicError(w, status, errType, message)
		return
	}
	data, err := json.Marshal(anthropicErrorEnvelope{
		Type:  "error",
		Error: anthropicErrorPayload{Type: errType, Message: message},
	})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// writeAnthropicErrorOrAbort renders a failure on the NON-STREAMING leg,
// where the hold may already have spent the status (waired-agent#1314).
//
// Pre-commit it is writeAnthropicError, byte for byte.
//
// Post-commit there is nothing honest left to write. This dialect's
// non-streaming response is ONE Message object and has no in-band error
// member, so the two things that can go under a committed 200 were both
// measured against the shipping client (Claude Code 2.1.269,
// docs/knowledges/20260912/2130-nonstream-leg-held-only-by-committing.md):
//
//	the error envelope   "API returned an empty or malformed response
//	                      (HTTP 200) — check for a proxy or gateway
//	                      intercepting the request"
//	an unfinished body   "Connection to the API was lost (ECONNRESET).
//	                      This is usually temporary — try again."
//
// The first sends the reader after their own network for a fault that is
// this computer's engine; the second is true, and its advice is the advice
// we would give. So the response is aborted: http.ErrAbortHandler closes the
// connection without the terminating chunk and without a stack trace, and
// rr.fail has already recorded the real status and reason, which is what
// keeps the event ring honest about a turn the client saw as a 200
// (waired-agent#538).
func writeAnthropicErrorOrAbort(w http.ResponseWriter, hold *engineHold, status int, errType, message string) {
	if !hold.committed() {
		writeAnthropicError(w, status, errType, message)
		return
	}
	abortHeldResponse("leg", "anthropic-nonstream", "status", status, "type", errType, "message", message)
}

// writeAnthropicMessageOrBody writes the completed turn on the non-streaming
// leg. Pre-commit that is writeJSON; post-commit the headers and the status
// are already on the wire, so only the body is left to write — appended to
// the pad frames, which JSON's grammar makes insignificant.
func writeAnthropicMessageOrBody(w http.ResponseWriter, hold *engineHold, out AnthropicResponse) {
	if !hold.committed() {
		writeJSON(w, http.StatusOK, out)
		return
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		// The turn cannot be delivered and the status is spent. Same
		// reasoning as writeAnthropicErrorOrAbort: a body the client
		// cannot parse would send them after their own proxy.
		abortHeldResponse("leg", "anthropic-nonstream", "err", err,
			"note", "the completed turn could not be encoded")
	}
	_, _ = w.Write(encoded)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// writeAnthropicStreamHeaders commits an Anthropic SSE response. Shared by
// the keepalive and by proxyAnthropicStream so one leg cannot drift from the
// other on the headers a streaming client depends on.
func writeAnthropicStreamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

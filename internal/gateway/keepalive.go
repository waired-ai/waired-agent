package gateway

import (
	"context"
	"encoding/json"
	"fmt"
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

// sseKeepalive holds a streaming response open while the engine has produced
// nothing at all (waired-agent#837).
//
// Lifetime is declared rather than implicit: it starts on the first TICK —
// never at t=0 — and ends at stop(reason), and both ends are logged. A turn
// whose engine answers inside one interval therefore writes nothing and is
// byte-identical to the same turn before this existed.
//
// It is never capped. This runs only on a leg with nowhere else to send the
// turn, and a stream that spoke and then went silent again is worse than one
// that never spoke.
//
// Concurrency contract: between start and stop, the keepalive OWNS w. stop
// takes the same mutex the ticker writes under and latches, so no write is in
// flight once it returns — only then may the caller write the stream.
type sseKeepalive struct {
	mu        sync.Mutex
	w         http.ResponseWriter
	flusher   http.Flusher
	stopped   bool
	frames    int
	started   time.Time
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
	onCommit  func()
	logFields []any
}

// startSSEKeepalive arms the keepalive on w and returns it. A nil return is
// never produced: callers hold the value and call stop unconditionally, and
// every method is nil-safe so a leg that armed nothing can share one code
// path with a leg that did.
//
// onCommit runs once, under the lock, immediately before the first frame
// reaches the wire — the moment the response status stops being ours to
// choose.
func startSSEKeepalive(ctx context.Context, w http.ResponseWriter, every time.Duration, onCommit func(), logFields ...any) *sseKeepalive {
	if every <= 0 {
		return nil
	}
	kctx, cancel := context.WithCancel(ctx)
	k := &sseKeepalive{
		w:         w,
		stopped:   false,
		started:   time.Now(),
		cancel:    cancel,
		done:      make(chan struct{}),
		onCommit:  onCommit,
		logFields: logFields,
	}
	k.flusher, _ = w.(http.Flusher)
	go k.run(kctx, every)
	return k
}

func (k *sseKeepalive) run(ctx context.Context, every time.Duration) {
	defer close(k.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !k.tick() {
				return
			}
		}
	}
}

// tick writes one frame. It reports false once the keepalive is finished,
// either because stop ran or because the client is gone.
func (k *sseKeepalive) tick() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return false
	}
	if k.frames == 0 {
		// The response status stops being ours here. Same three headers
		// proxyAnthropicStream sets on the normal path, hoisted so both
		// callers write one set.
		writeAnthropicStreamHeaders(k.w)
		if k.onCommit != nil {
			k.onCommit()
		}
	}
	if _, err := fmt.Fprint(k.w, keepaliveFrame); err != nil {
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
func (k *sseKeepalive) committed() bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.frames > 0
}

// stop ends the keepalive and returns only once no write can still be in
// flight, so the caller may write the stream itself. Idempotent and nil-safe.
func (k *sseKeepalive) stop(reason string) {
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
				"waited_ms", time.Since(k.started).Milliseconds(),
				"frames", frames,
			}, k.logFields...)...)
	})
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
func writeAnthropicErrorOrEvent(w http.ResponseWriter, hold *sseKeepalive, status int, errType, message string) {
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

// writeAnthropicStreamHeaders commits an Anthropic SSE response. Shared by
// the keepalive and by proxyAnthropicStream so one leg cannot drift from the
// other on the headers a streaming client depends on.
func writeAnthropicStreamHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

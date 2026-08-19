package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/observability"
)

// Time-to-first-token (waired-agent#874).
//
// PRODUCT CONTRACT, ratified by waired-agent#874 and the measurement in
// waired-agent#838: TTFTMs is the wait from request entry to the instant
// the ENGINE produced its first token, of any kind. Three of the tests
// below exist to kill three specific wrong implementations rather than
// merely to exercise the right one:
//
//	M1  the stamp is missing entirely            -> TTFTMs stays 0
//	M2  the stamp is taken at response HEADERS   -> TTFTMs collapses to ~0
//	M3  the stamp is taken at the END of the run -> TTFTMs equals LatencyMs
//
// M2 is the important one: headers are the instant the code measured
// before this change (the "ttfb_ms" debug lines and the #757 budget), so
// it is the mistake a future edit is most likely to reintroduce.

// ttftDelays describes an engine's timing so one fixture covers every
// case. Zero values mean "no wait".
type ttftDelays struct {
	beforeHeaders time.Duration // headers withheld this long
	beforeFirst   time.Duration // headers flushed, then the first delta waits
	afterFirst    time.Duration // first delta sent, then the rest waits
}

// ttftEngine streams `first` as its opening delta and `rest` as a closing
// one, with the delays applied around them. first is written verbatim as
// the value of "delta", so a test can open with content, reasoning or a
// tool call.
func ttftEngine(first, rest string, d ttftDelays) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if d.beforeHeaders > 0 {
			select {
			case <-time.After(d.beforeHeaders):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		if f != nil {
			f.Flush()
		}
		if d.beforeFirst > 0 {
			select {
			case <-time.After(d.beforeFirst):
			case <-r.Context().Done():
				return
			}
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":` + first + `}]}` + "\n\n"))
		if f != nil {
			f.Flush()
		}
		if d.afterFirst > 0 {
			select {
			case <-time.After(d.afterFirst):
			case <-r.Context().Done():
				return
			}
		}
		if rest != "" {
			_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":` + rest + `}]}` + "\n\n"))
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f != nil {
			f.Flush()
		}
	})
	return httptest.NewServer(mux)
}

// driveTTFT runs one streamed Anthropic turn against engineURL through a
// real requestRec and returns the event the recorder saw. It goes
// through startRequest/finish rather than constructing the event, so the
// clock origin under test is the production one.
func driveTTFT(t *testing.T, engineURL string) observability.RequestEvent {
	t.Helper()
	rec := &captureRecorder{}
	h := NewHandlerSet(Deps{HTTPClient: http.DefaultClient, Recorder: rec})
	rr := h.startRequest(nil, "anthropic")
	rr.ev.Model = "qwen3-8b-instruct" // finish() drops events with no model

	w := httptest.NewRecorder()
	h.proxyAnthropicStream(context.Background(), http.DefaultClient, engineURL,
		[]byte(ttfbStreamBody), "waired/default", nil, w, 0, localSel, rr, nil)
	rr.finish()

	got := rec.requestsSnapshot()
	if len(got) != 1 {
		t.Fatalf("recorded %d request events, want 1", len(got))
	}
	return got[0]
}

// M1: the stamp exists at all, and never exceeds the whole request.
func TestAnthropicStream_RecordsTTFT(t *testing.T) {
	engine := ttftEngine(`{"role":"assistant","content":"Hi"}`, "",
		ttftDelays{beforeHeaders: 200 * time.Millisecond})
	defer engine.Close()

	ev := driveTTFT(t, engine.URL)
	if ev.TTFTMs < 150 {
		t.Errorf("TTFTMs = %d, want >= 150 (engine held the turn 200ms) — is the stamp wired up?", ev.TTFTMs)
	}
	if ev.TTFTMs > ev.LatencyMs {
		t.Errorf("TTFTMs = %d > LatencyMs = %d; the first token cannot arrive after the last", ev.TTFTMs, ev.LatencyMs)
	}
}

// M2: the instant is the first TOKEN, not the response headers. The
// engine flushes headers immediately and only then waits, so a
// headers-based implementation reports ~0 here.
func TestAnthropicStream_TTFTIsFirstTokenNotHeaders(t *testing.T) {
	engine := ttftEngine(`{"role":"assistant","content":"Hi"}`, "",
		ttftDelays{beforeFirst: 300 * time.Millisecond})
	defer engine.Close()

	ev := driveTTFT(t, engine.URL)
	if ev.TTFTMs < 250 {
		t.Errorf("TTFTMs = %d, want >= 250: headers were flushed immediately and the first token withheld 300ms, "+
			"so this value means the stamp is being taken at response headers", ev.TTFTMs)
	}
}

// M3: the instant is the FIRST token, not the last. Everything after it
// is decode and must fall outside TTFT.
func TestAnthropicStream_TTFTExcludesDecode(t *testing.T) {
	engine := ttftEngine(`{"role":"assistant","content":"Hi"}`, `{"content":" there"}`,
		ttftDelays{afterFirst: 300 * time.Millisecond})
	defer engine.Close()

	ev := driveTTFT(t, engine.URL)
	if ev.TTFTMs > 150 {
		t.Errorf("TTFTMs = %d, want < 150: the first token was immediate", ev.TTFTMs)
	}
	if ev.LatencyMs-ev.TTFTMs < 250 {
		t.Errorf("LatencyMs-TTFTMs = %d, want >= 250: 300ms of decode followed the first token, "+
			"so this means the stamp is being taken at the end of the stream", ev.LatencyMs-ev.TTFTMs)
	}
}

// A thinking model emits reasoning before any content, and that trace
// reaches the client unsieved — it is the user's first visible output,
// so it is where prefill ended. Stamping only on content would report
// the whole thinking phase as prefill.
func TestAnthropicStream_TTFTCountsTheReasoningDelta(t *testing.T) {
	engine := ttftEngine(`{"role":"assistant","reasoning":"hmm"}`, `{"content":"Hi"}`,
		ttftDelays{afterFirst: 300 * time.Millisecond})
	defer engine.Close()

	ev := driveTTFT(t, engine.URL)
	if ev.TTFTMs > 150 {
		t.Errorf("TTFTMs = %d, want < 150: reasoning arrived immediately and content 300ms later, "+
			"so this means only content deltas are being counted", ev.TTFTMs)
	}
}

// A turn that goes straight to a tool call paid the same prefill. Coding
// agents produce mostly these, so excluding them would make the majority
// of the measured population invisible.
func TestAnthropicStream_TTFTCountsAToolCallOnlyTurn(t *testing.T) {
	engine := ttftEngine(
		`{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]}`,
		"", ttftDelays{beforeFirst: 200 * time.Millisecond})
	defer engine.Close()

	ev := driveTTFT(t, engine.URL)
	if ev.TTFTMs < 150 {
		t.Errorf("TTFTMs = %d, want >= 150: the only delta in this turn was a tool call, "+
			"so a zero means tool-call deltas are not counted as tokens", ev.TTFTMs)
	}
}

// A bare role marker carries no token. Stamping on it would report the
// engine's greeting chunk as the end of prefill.
func TestAnthropicStream_TTFTIgnoresARoleOnlyDelta(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"role":"assistant"}}]}` + "\n\n"))
		if f != nil {
			f.Flush()
		}
		select {
		case <-time.After(300 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if f != nil {
			f.Flush()
		}
	})
	engine := httptest.NewServer(mux)
	defer engine.Close()

	ev := driveTTFT(t, engine.URL)
	if ev.TTFTMs < 250 {
		t.Errorf("TTFTMs = %d, want >= 250: the first chunk was a role marker with no token, "+
			"so this means an empty delta is being counted", ev.TTFTMs)
	}
}

// firstTokenMs renders an observed wait, and zero on this event means
// "not observed" — so a sub-millisecond observation must not be reported
// as an absent one. Table test on the pure helper, so the clamp is
// checked without racing a real clock.
func TestFirstTokenMs_NeverReportsAnObservationAsZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want uint32
	}{
		{"zero", 0, 1},
		{"sub-millisecond", 900 * time.Microsecond, 1},
		{"just over", 1400 * time.Microsecond, 1},
		{"whole milliseconds", 412 * time.Millisecond, 412},
		{"seconds", 41*time.Second + 510*time.Millisecond, 41510},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstTokenMs(tc.in); got != tc.want {
				t.Errorf("firstTokenMs(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// setFirstToken latches: the first call wins, so a retry inside one
// request cannot move the instant the human actually saw something.
func TestSetFirstToken_LatchesOnTheFirstCall(t *testing.T) {
	rr := &requestRec{start: time.Now().Add(-200 * time.Millisecond)}
	rr.setFirstToken()
	first := rr.ev.TTFTMs
	if first < 150 {
		t.Fatalf("TTFTMs = %d, want >= 150", first)
	}
	time.Sleep(50 * time.Millisecond)
	rr.setFirstToken()
	if rr.ev.TTFTMs != first {
		t.Errorf("TTFTMs moved on the second call: %d -> %d", first, rr.ev.TTFTMs)
	}
}

// A nil record is the direct-call shape the stream tests use; the
// siblings are all nil-safe and this must be too.
func TestSetFirstToken_NilRecordIsSafe(t *testing.T) {
	var rr *requestRec
	rr.setFirstToken()
}

// RECORD OF TODAY'S BEHAVIOUR, not a contract: these legs cannot observe
// a first-token instant, so they leave the field unobserved rather than
// reporting a different quantity under the same name. The OpenAI
// passthrough forwards bytes without parsing them; a non-streamed
// response has no first token distinct from its last.
//
// It is here to stop a later change quietly filling the field with
// response-header time, which would make the metric mean two things.
func TestGateway_TTFTNotObservedWhereItCannotBe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		payload map[string]any
	}{
		{"openai non-stream", "/v1/chat/completions", map[string]any{
			"model": "waired/default", "messages": []map[string]string{{"role": "user", "content": "hi"}}}},
		{"openai stream", "/v1/chat/completions", map[string]any{
			"model": "waired/default", "stream": true,
			"messages": []map[string]string{{"role": "user", "content": "hi"}}}},
		{"anthropic non-stream", "/anthropic/v1/messages", map[string]any{
			"model": "waired/default", "max_tokens": 64,
			"messages": []map[string]string{{"role": "user", "content": "hi"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := meteringEngine(t, nil)
			defer engine.Close()
			rec := &captureRecorder{}
			gw := newMeteringGateway(t, engine.URL, rec, nil)

			if w := postJSON(t, gw, tc.path, tc.payload); w.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
			}
			evs := rec.requestsSnapshot()
			if len(evs) != 1 {
				t.Fatalf("recorded %d request events, want 1", len(evs))
			}
			if evs[0].TTFTMs != 0 {
				t.Errorf("TTFTMs = %d, want 0: this leg cannot observe a first-token instant, "+
					"so a value here is a different quantity wearing the same name", evs[0].TTFTMs)
			}
		})
	}
}

// The Anthropic STREAMING leg through the full server does observe it —
// the companion to the table above, so "not observed" is never mistaken
// for "never observed anywhere".
func TestGateway_TTFTObservedOnTheAnthropicStreamingLeg(t *testing.T) {
	engine := meteringEngine(t, nil)
	defer engine.Close()
	rec := &captureRecorder{}
	gw := newMeteringGateway(t, engine.URL, rec, nil)

	w := postJSON(t, gw, "/anthropic/v1/messages", map[string]any{
		"model": "waired/default", "max_tokens": 64, "stream": true,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	evs := rec.requestsSnapshot()
	if len(evs) != 1 {
		t.Fatalf("recorded %d request events, want 1", len(evs))
	}
	if evs[0].TTFTMs == 0 {
		t.Errorf("TTFTMs = 0 on the Anthropic streaming leg, which is the one leg that can see the instant")
	}
}

package download

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"testing/iotest"
	"time"
)

// progressReader must throttle emits to progressEmitEvery, keep completed
// monotonic, report the rate as -1 until two distinct-time samples exist,
// and guarantee a final emit at EOF.
func TestProgressReader_ThrottleAndFinalEmit(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 10)
	clock := time.Unix(0, 0)
	type event struct{ completed, total, rate int64 }
	var events []event
	pr := &progressReader{
		r:     iotest.OneByteReader(bytes.NewReader(body)),
		total: int64(len(body)),
		now: func() time.Time {
			t := clock
			clock = clock.Add(150 * time.Millisecond) // one tick per 1-byte read
			return t
		},
		onProgress: func(c, tot, r int64) { events = append(events, event{c, tot, r}) },
	}
	got, err := io.ReadAll(pr)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("ReadAll: err=%v got %d bytes, want %d", err, len(got), len(body))
	}
	if len(events) < 2 || len(events) >= len(body) {
		// 150 ms per read against the 200 ms throttle: roughly every other
		// read emits, plus the final EOF emit — never one emit per read.
		t.Fatalf("events = %d, want throttled (2..%d): %+v", len(events), len(body)-1, events)
	}
	if events[0].rate != -1 {
		t.Errorf("first emit rate = %d, want -1 (unknown until a second sample)", events[0].rate)
	}
	prev := int64(0)
	for _, ev := range events {
		if ev.completed < prev {
			t.Fatalf("completed regressed: %+v", events)
		}
		prev = ev.completed
		if ev.total != int64(len(body)) {
			t.Errorf("total = %d, want %d", ev.total, len(body))
		}
	}
	last := events[len(events)-1]
	if last.completed != int64(len(body)) {
		t.Errorf("final emit completed = %d, want %d (EOF must flush)", last.completed, len(body))
	}
	if last.rate <= 0 {
		t.Errorf("final rate = %d, want > 0 once samples span time", last.rate)
	}
}

// Fetch must stream byte progress with the response Content-Length as
// total, ending on completed == total, and land every body byte in w.
func TestFetch_StreamsProgress(t *testing.T) {
	body := bytes.Repeat([]byte("y"), 256<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	var gotBytes, totals []int64
	n, err := Fetch(context.Background(), nil, srv.URL, &buf, nil, func(c, tot, _ int64) {
		gotBytes, totals = append(gotBytes, c), append(totals, tot)
	})
	if err != nil || n != int64(len(body)) || buf.Len() != len(body) {
		t.Fatalf("Fetch: err=%v n=%d buf=%d bytes, want %d", err, n, buf.Len(), len(body))
	}
	if len(gotBytes) == 0 {
		t.Fatal("no progress emitted")
	}
	if last := gotBytes[len(gotBytes)-1]; last != int64(len(body)) {
		t.Errorf("final completed = %d, want %d", last, len(body))
	}
	for _, tot := range totals {
		if tot != int64(len(body)) {
			t.Errorf("total = %d, want %d (Content-Length)", tot, len(body))
		}
	}
}

// Without a Content-Length (chunked response) the total must degrade to -1
// while byte progress still streams.
func TestFetch_UnknownLength(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 64<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.(http.Flusher).Flush() // force chunked: no Content-Length
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	var events int
	var lastCompleted, lastTotal int64
	n, err := Fetch(context.Background(), nil, srv.URL, &buf, nil, func(c, tot, _ int64) {
		events++
		lastCompleted, lastTotal = c, tot
	})
	if err != nil || n != int64(len(body)) {
		t.Fatalf("Fetch: err=%v n=%d, want %d", err, n, len(body))
	}
	if events == 0 {
		t.Fatal("no progress emitted")
	}
	if lastTotal != -1 {
		t.Errorf("total = %d, want -1 for an unknown length", lastTotal)
	}
	if lastCompleted != int64(len(body)) {
		t.Errorf("final completed = %d, want %d", lastCompleted, len(body))
	}
}

// A non-200 status is an error and must not write anything to w.
func TestFetch_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	if _, err := Fetch(context.Background(), nil, srv.URL, &buf, nil, nil); err == nil {
		t.Fatal("expected an error for HTTP 404")
	}
	if buf.Len() != 0 {
		t.Errorf("w received %d bytes on an error response, want 0", buf.Len())
	}
}

// A nil onProgress must stream without callbacks (no panic).
func TestFetch_NilProgress(t *testing.T) {
	body := []byte("ok")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	var buf bytes.Buffer
	n, err := Fetch(context.Background(), nil, srv.URL, &buf, nil, nil)
	if err != nil || n != int64(len(body)) || !bytes.Equal(buf.Bytes(), body) {
		t.Fatalf("Fetch: err=%v n=%d buf=%q, want %q", err, n, buf.Bytes(), body)
	}
}

// A body that goes quiet mid-transfer must be abandoned on the stall bound
// and reported as a stall, not left hanging until some outer deadline (#189).
func TestFetch_StallsOut(t *testing.T) {
	restore := FetchStallTimeout
	FetchStallTimeout = 150 * time.Millisecond
	t.Cleanup(func() { FetchStallTimeout = restore })

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1024")
		_, _ = w.Write(bytes.Repeat([]byte("a"), 16))
		w.(http.Flusher).Flush()
		<-release // then stop sending, without closing the connection
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	var buf bytes.Buffer
	n, err := Fetch(context.Background(), nil, srv.URL, &buf, nil, nil)
	if !errors.Is(err, ErrStalled) {
		t.Fatalf("err = %v, want ErrStalled", err)
	}
	if n != 16 {
		t.Errorf("n = %d, want the 16 bytes that did arrive", n)
	}
}

// A slow but steady transfer must NOT be killed: every read resets the
// countdown, which is the whole point of bounding on progress rather than
// on total elapsed time.
//
// The property is per-GAP, not per-transfer: what must hold is that no
// single interval between reads reaches the bound. Its predecessor sized
// the margin off total elapsed time instead ("takes ~5x the stall bound to
// finish") and wrote 50 ms gaps against a 100 ms bound — a 2x margin, drawn
// eleven times a run (headers-to-first-chunk, nine inter-chunk gaps, and a
// trailing one because the sleep sat after the last write). One 50 ms sleep
// overshooting to 100 ms was enough, and a loaded darwin runner duly
// produced one (waired-agent#931).
//
// Sized against measurement, not intuition. Sleeping the writer's pacing on
// a box with 4x CPU oversubscription (14 cores, 56 spinning goroutines) and
// recording every gap:
//
//	pacing      p50      p99      max      gaps >= 100ms
//	50ms x 10   60.7ms   86.9ms   179.0ms  19/3000 = 0.63%
//
// 0.63% per gap over the predecessor's ten draws is a **6.2% failure rate
// per run** — which is exactly what "red once, green on rerun" looks like,
// and why it surfaced on the slowest leg first.
//
// Two things that measurement settles. Scheduler starvation is roughly
// ADDITIVE rather than proportional — 10 ms and 50 ms sleeps both came back
// ~190 ms late at the tail — so what protects this test is the absolute
// slack (bound minus gap), not their ratio; shrinking the gap buys almost
// nothing. And each gap is another draw from that tail, so once the slack is
// fixed, fewer large gaps beat many small ones.
//
// Hence 50 ms gaps against a 1 s bound: 950 ms of slack against a worst
// observed delay of 533 ms, where the predecessor had 50 ms of slack against
// the same distribution. At the intermediate 750 ms bound the worst gap
// measured was 583 ms — 0 exceedances in 6000, but close enough to the bound
// to be worth the extra 0.2 s.
//
// #384's rule applies: this is an upper-bound assertion, where sleep
// overshoot makes the condition FALSER — unlike TestFetch_StallsOut above,
// which is a lower bound and needs no such margin.
func TestFetch_SlowButSteadySurvives(t *testing.T) {
	restore := FetchStallTimeout
	FetchStallTimeout = time.Second
	t.Cleanup(func() { FetchStallTimeout = restore })

	// 23 gaps x 50 ms = 1.15 s, so the transfer outlives the bound by 15%
	// even when every sleep is exact — which is what the elapsed assertion
	// at the end requires, and why the chunk count may not be trimmed.
	const (
		chunks   = 24
		chunkGap = 50 * time.Millisecond
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i < chunks; i++ {
			// Before the write, not after: a trailing gap delays only EOF,
			// so it can trip the bound without ever testing a reset.
			if i > 0 {
				time.Sleep(chunkGap)
			}
			_, _ = w.Write([]byte("chunk"))
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()

	var buf bytes.Buffer
	started := time.Now()
	n, err := Fetch(context.Background(), nil, srv.URL, &buf, nil, nil)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Fetch: %v (a slow-but-alive transfer must not trip the stall bound)", err)
	}
	if want := int64(chunks * len("chunk")); n != want {
		t.Errorf("n = %d, want %d", n, want)
	}
	// Anti-vacuity, and the reason the chunk count may not be trimmed for
	// speed: a transfer that finishes inside the bound never asks the guard
	// anything, so without this the test can go green by proving nothing.
	if elapsed <= FetchStallTimeout {
		t.Fatalf("the transfer took %v, inside the %v bound — no reset was ever "+
			"needed, so this test proved nothing", elapsed, FetchStallTimeout)
	}
}

// A transfer that completes must be reported as complete, even if the stall
// timer fires in the window between io.Copy returning and Fetch reading the
// guard. io.Copy returns a nil error only on clean EOF, so the bytes are all
// there; announcing ErrStalled would throw away a download that worked.
//
// Record of today's behaviour rather than a product contract: at the shipped
// FetchStallTimeout (120 s) this window needs a 120 s deschedule between two
// adjacent statements and is unreachable. It is reachable at the bounds these
// tests use, which is where it was found.
func TestFetch_CompletedTransferIsNotReportedAsStalled(t *testing.T) {
	restore := FetchStallTimeout
	FetchStallTimeout = 20 * time.Millisecond
	t.Cleanup(func() { FetchStallTimeout = restore })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("done"))
	}))
	defer srv.Close()

	// A sink that stalls AFTER taking the whole body. By the time it
	// returns, every byte has arrived and the next read is a buffered EOF,
	// so io.Copy reports success — while the guard, armed before io.Copy
	// and stopped only by a defer that runs after the return values are
	// built, has long since fired.
	sink := &slowWriter{delay: 4 * FetchStallTimeout}
	n, err := Fetch(context.Background(), nil, srv.URL, sink, nil, nil)
	if err != nil {
		t.Fatalf("Fetch: %v — a completed transfer was reported as a failure", err)
	}
	if got := sink.buf.String(); got != "done" {
		t.Errorf("body = %q, want %q", got, "done")
	}
	if n != 4 {
		t.Errorf("n = %d, want 4", n)
	}
}

// slowWriter takes the bytes immediately and then blocks, which is how a
// test puts a delay strictly between "the body has all arrived" and "io.Copy
// returns".
type slowWriter struct {
	delay time.Duration
	buf   bytes.Buffer
}

func (s *slowWriter) Write(p []byte) (int, error) {
	n, err := s.buf.Write(p)
	time.Sleep(s.delay)
	return n, err
}

package download

import (
	"bytes"
	"context"
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

package download

// HTTP download with streamed byte progress. Extracted from the Linux
// bundled-Ollama installer (internal/runtime/ollama_install.go, PR #608)
// so every multi-hundred-MB fetch — the Linux tarball and the macOS
// Ollama.app zip (#615) — reports the same throttled
// completed/total/rate stream instead of sitting silent for the whole
// transfer.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Fetch GETs url and streams the response body into w, emitting
// throttled onProgress callbacks (completed bytes, total from
// Content-Length or -1 when unknown, smoothed bytes/sec or -1 before the
// first rate sample). client nil means http.DefaultClient; now nil means
// time.Now (a seam for deterministic tests); onProgress may be nil. A
// non-2xx-OK status is an error and writes nothing to w. Returns the
// byte count written to w.
func Fetch(ctx context.Context, client *http.Client, url string, w io.Writer,
	now func() time.Time, onProgress func(completed, total, bytesPerSec int64)) (int64, error) {
	if client == nil {
		client = http.DefaultClient
	}
	if now == nil {
		now = time.Now
	}
	// Bound the transfer on time-without-progress rather than on total
	// elapsed time. A whole-request cap kills a healthy slow download (the
	// 15-minute install deadline killed a ~1.43 GB fetch at 1.5 MB/s a
	// minute short of finishing, #189); a stall bound only fires when bytes
	// genuinely stop arriving, so a slow link is left alone and a wedged
	// connection is not.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var body io.Reader = resp.Body
	if onProgress != nil {
		body = &progressReader{r: resp.Body, total: resp.ContentLength, now: now, onProgress: onProgress}
	}
	guard := newStallGuard(FetchStallTimeout, cancel)
	defer guard.stop()
	n, err := io.Copy(w, &stallReader{r: body, guard: guard})
	if guard.fired() {
		return n, fmt.Errorf("%w after %s (%d bytes received)", ErrStalled, FetchStallTimeout, n)
	}
	return n, err
}

// ErrStalled is returned when a download makes no progress for
// FetchStallTimeout. Distinguishable so callers can say "the download
// stalled" instead of collapsing it into a generic failure.
var ErrStalled = errors.New("download stalled")

// FetchStallTimeout is how long a transfer may make no progress before it
// is abandoned. It matches the per-read stall timeout the PowerShell
// downloader has always used (ollama-windows.ps1's ReadWriteTimeout), so
// all three OSes give up at the same point. A var so tests can shorten it.
var FetchStallTimeout = 120 * time.Second

// stallGuard cancels the request context when it is not reset within the
// timeout. Reset lives on the read path, which is blocked inside the socket
// read while stalled, so the deadline has to be enforced from a timer
// rather than checked inline.
type stallGuard struct {
	timeout time.Duration
	timer   *time.Timer
	tripped atomic.Bool
}

func newStallGuard(timeout time.Duration, cancel context.CancelFunc) *stallGuard {
	g := &stallGuard{timeout: timeout}
	g.timer = time.AfterFunc(timeout, func() {
		g.tripped.Store(true)
		cancel()
	})
	return g
}

// reset restarts the countdown after real progress. Once tripped it stays
// tripped: the context is already cancelled and a late read must not
// resurrect the transfer.
func (g *stallGuard) reset() {
	if g.tripped.Load() {
		return
	}
	g.timer.Reset(g.timeout)
}

func (g *stallGuard) stop()       { g.timer.Stop() }
func (g *stallGuard) fired() bool { return g.tripped.Load() }

// stallReader feeds the guard from the read path.
type stallReader struct {
	r     io.Reader
	guard *stallGuard
}

func (s *stallReader) Read(b []byte) (int, error) {
	n, err := s.r.Read(b)
	if n > 0 {
		s.guard.reset()
	}
	return n, err
}

// progressEmitEvery throttles progressReader callbacks: frequent enough
// for a lively TTY bar, sparse enough to stay negligible next to the
// transfer itself.
const progressEmitEvery = 200 * time.Millisecond

// progressRateWindow is how far back the rate smoothing looks. Short
// enough to track real throughput changes, long enough that per-read
// jitter doesn't make the displayed rate flicker.
const progressRateWindow = 3 * time.Second

// progressReader wraps a download body, counting bytes and emitting
// throttled onProgress callbacks with a rate smoothed over a short rolling
// window of samples. It reports total as received (typically the response
// Content-Length; -1 when unknown) and guarantees a final emit at EOF so
// renderers can close the bar at 100%.
type progressReader struct {
	r          io.Reader
	total      int64
	now        func() time.Time
	onProgress func(completed, total, bytesPerSec int64)

	completed int64
	lastEmit  time.Time
	samples   []progressSample
}

type progressSample struct {
	at    time.Time
	bytes int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.completed += int64(n)
		now := p.now()
		if p.lastEmit.IsZero() || now.Sub(p.lastEmit) >= progressEmitEvery {
			p.emit(now)
		}
	}
	if err == io.EOF && p.completed > 0 {
		p.emit(p.now())
	}
	return n, err
}

// emit records a rate sample at now and fires onProgress.
func (p *progressReader) emit(now time.Time) {
	p.lastEmit = now
	p.samples = append(p.samples, progressSample{at: now, bytes: p.completed})
	// Drop samples that fell out of the window, but keep one older sample
	// as the anchor — pruning to a single sample would flip the rate back
	// to "unknown" after a read gap instead of averaging across it.
	cutoff := now.Add(-progressRateWindow)
	for len(p.samples) > 1 && p.samples[1].at.Before(cutoff) {
		p.samples = p.samples[1:]
	}
	p.onProgress(p.completed, p.total, windowedRate(p.samples))
}

// windowedRate averages the byte delta across the sample window; -1 until
// two distinct-time samples exist (rate unknown yet).
func windowedRate(samples []progressSample) int64 {
	first, last := samples[0], samples[len(samples)-1]
	secs := last.at.Sub(first.at).Seconds()
	if secs <= 0 {
		return -1
	}
	return int64(float64(last.bytes-first.bytes) / secs)
}

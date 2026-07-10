package download

// HTTP download with streamed byte progress. Extracted from the Linux
// bundled-Ollama installer (internal/runtime/ollama_install.go, PR #608)
// so every multi-hundred-MB fetch — the Linux tarball and the macOS
// Ollama.app zip (#615) — reports the same throttled
// completed/total/rate stream instead of sitting silent for the whole
// transfer.

import (
	"context"
	"fmt"
	"io"
	"net/http"
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
	return io.Copy(w, body)
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

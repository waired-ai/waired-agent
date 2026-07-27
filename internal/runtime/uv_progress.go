package runtime

import (
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/download"
)

// uv's own progress bars are drawn with indicatif, which hides itself
// when stderr is not a terminal — and DefaultInstallRunner gives uv a
// pipe, not a pty. So during a browser-driven vLLM setup uv renders no
// bar and no percentage at all, and extractInstallPercent finds nothing
// to extract: the wizard AND the operator's terminal both sat silent for
// the whole ~6 GB venv build (waired-agent#255).
//
// What uv does emit through a pipe is a pair of plain log lines per
// package it actually downloads:
//
//	Downloading torch (506.1MiB)
//	 Downloaded torch
//
// Those carry the one thing the §7 `engine_download` row needs and the
// percentage never could: real byte counts, from uv's own figures rather
// than an estimate of ours. uvDownloadTracker is the reader.
//
// Only packages above uv's own size threshold are announced — 63 of the
// 190 vLLM resolves on a real run — so the total is the download that
// matters rather than every wheel. A package that is not announced is
// not counted on either side, which keeps completed and total in the
// same units at every moment.
//
// This file is deliberately untagged even though vllm_install.go is
// linux-only: the format is a contract with an external tool, and a
// parser that can only be tested on one OS is a parser that is tested
// nightly at best (the repo's cross-OS parity rule, same reasoning as
// cmd/waired/ollama_progress_line.go).
const (
	uvDownloadingPrefix = "Downloading "
	uvDownloadedPrefix  = "Downloaded "
	// uvPreparedPrefix marks the end of the transfer phase. uv prints it
	// once every wheel is fetched and unpacked, so it is the authority on
	// "the download is over" — a bar parked at 97% because one small
	// announcement was missed reads as a stall.
	uvPreparedPrefix = "Prepared "
)

// uvRateWindow is how far back the reported transfer rate looks.
//
// uv's completion events are lumpy by nature (one per wheel, and the
// wheels range from 2 MiB to 506 MiB), so an instantaneous delta between
// two adjacent events measures the size of the last wheel, not the line
// speed. A trailing window averages across several of them.
const uvRateWindow = 20 * time.Second

type uvRateSample struct {
	at        time.Time
	completed int64
}

// uvDownloadTracker accumulates uv's per-package announcements into the
// (completed, total, rate) triple the setup wizard's byte bar is drawn
// from. Not safe for concurrent use; it is driven from the single
// goroutine that scans the child's output.
type uvDownloadTracker struct {
	sizes     map[string]int64 // package -> announced download size
	done      map[string]bool  // packages whose transfer has finished
	announced int64            // sum of every announced size
	completed int64            // sum of the announced sizes that finished
	samples   []uvRateSample
	prepared  bool
}

func newUVDownloadTracker() *uvDownloadTracker {
	return &uvDownloadTracker{
		sizes: map[string]int64{},
		done:  map[string]bool{},
	}
}

// Observe feeds one line of uv output to the tracker and reports whether
// the byte figures moved. Lines that are not download announcements are
// ignored, so the caller can hand it every line unconditionally.
func (t *uvDownloadTracker) Observe(line string, now time.Time) bool {
	// " Downloaded torch" arrives with a leading space; "Downloading
	// torch (506.1MiB)" does not. Trim once and test the longer prefix
	// first, since "Downloading" also starts with "Download".
	s := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(s, uvDownloadingPrefix):
		name, size, ok := parseUVDownloading(s)
		if !ok {
			return false
		}
		if _, seen := t.sizes[name]; seen {
			return false
		}
		t.sizes[name] = size
		t.announced += size
		t.sample(now)
		return true

	case strings.HasPrefix(s, uvDownloadedPrefix):
		name := strings.TrimSpace(strings.TrimPrefix(s, uvDownloadedPrefix))
		size, announced := t.sizes[name]
		// A completion for a package that was never announced carries no
		// bytes we could attribute, and counting it as zero would be a
		// step the bar does not take. Ignore it rather than invent one.
		if !announced || t.done[name] {
			return false
		}
		t.done[name] = true
		t.completed += size
		t.sample(now)
		return true

	case strings.HasPrefix(s, uvPreparedPrefix):
		// Every wheel is in. Close the gap left by anything uv fetched
		// without announcing, so the row finishes on 100%.
		if t.prepared || t.announced == 0 {
			return false
		}
		t.prepared = true
		if t.completed >= t.announced {
			return false
		}
		t.completed = t.announced
		t.sample(now)
		return true
	}
	return false
}

// Snapshot returns the current figures. total is 0 until uv announces
// its first download, which is the wire's "size unknown" — the wizard
// renders that indeterminate rather than drawing a bar it would have to
// resize a moment later.
func (t *uvDownloadTracker) Snapshot() (completed, total, bps int64) {
	return t.completed, t.announced, t.rate()
}

func (t *uvDownloadTracker) sample(now time.Time) {
	t.samples = append(t.samples, uvRateSample{at: now, completed: t.completed})
	// Drop everything that fell out of the window, but keep the last one
	// that did: it is the left edge the rate is measured from.
	cut := 0
	for i, s := range t.samples {
		if now.Sub(s.at) <= uvRateWindow {
			break
		}
		cut = i
	}
	if cut > 0 {
		t.samples = t.samples[cut:]
	}
}

// rate is the average over the trailing window. 0 means "not enough
// samples yet" — the same value the wire uses for unknown, so nothing is
// lost by not distinguishing it from a stall (a stall is read off
// completed not advancing, per waired-agent#197).
func (t *uvDownloadTracker) rate() int64 {
	if len(t.samples) < 2 {
		return 0
	}
	first, last := t.samples[0], t.samples[len(t.samples)-1]
	elapsed := last.at.Sub(first.at)
	if elapsed <= 0 {
		return 0
	}
	delta := last.completed - first.completed
	if delta <= 0 {
		return 0
	}
	return int64(float64(delta) / elapsed.Seconds())
}

// parseUVDownloading splits "Downloading <name> (<size><unit>)". The
// size token has no space in it ("506.1MiB"), and the name is taken up
// to the last " (" so a package whose name contains one would still
// resolve.
func parseUVDownloading(s string) (name string, size int64, ok bool) {
	rest := strings.TrimPrefix(s, uvDownloadingPrefix)
	if !strings.HasSuffix(rest, ")") {
		return "", 0, false
	}
	open := strings.LastIndex(rest, " (")
	if open <= 0 {
		return "", 0, false
	}
	name = strings.TrimSpace(rest[:open])
	size = parseUVSize(rest[open+2 : len(rest)-1])
	if name == "" || size <= 0 {
		return "", 0, false
	}
	return name, size, true
}

// parseUVSize converts uv's unspaced size token ("506.1MiB", "4.2KiB")
// into bytes. The unit table itself is download.ParseSize's, which
// already handles both the binary and decimal families and is what the
// ollama progress reader uses — one place decides what "MiB" means.
func parseUVSize(tok string) int64 {
	i := 0
	for i < len(tok) && (tok[i] == '.' || (tok[i] >= '0' && tok[i] <= '9')) {
		i++
	}
	if i == 0 || i == len(tok) {
		return 0
	}
	return download.ParseSize(tok[:i], tok[i:])
}

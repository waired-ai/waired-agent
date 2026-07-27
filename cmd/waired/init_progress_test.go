package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// The rate segment must render whenever the rate is known — including a
// stalled "(0 B/s)" — and be omitted only while unknown (< 0). Its absence
// used to make a slow-but-alive download look frozen.
func TestDrawDownloadLine_RateAlways(t *testing.T) {
	cases := []struct {
		name                    string
		pct                     int
		completed, total, speed int64
		want                    string
		absent                  string
	}{
		{"flowing", 50, 2_500_000_000, 5_000_000_000, 40_000_000, "50%  2.5 GB / 5.0 GB (40.0 MB/s)", ""},
		{"stalled", 50, 2_500_000_000, 5_000_000_000, 0, "(0 B/s)", ""},
		{"rate unknown yet", 50, 2_500_000_000, 5_000_000_000, -1, "2.5 GB / 5.0 GB", "/s)"},
		{"length unknown", -1, 300_000_000, -1, 12_000_000, "300.0 MB (12.0 MB/s)", "%"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			st := downloadLineState{lastPct: -1}
			drawDownloadLine(&buf, false, &st, "demo", c.pct, c.completed, c.total, c.speed)
			out := buf.String()
			if !strings.Contains(out, c.want) {
				t.Errorf("output %q missing %q", out, c.want)
			}
			if c.absent != "" && strings.Contains(out, c.absent) {
				t.Errorf("output %q should not contain %q", out, c.absent)
			}
		})
	}
}

// rateWindow: unknown until samples span time, then the windowed average —
// 0 across a stall (not a dropout) — and a reset when bytes regress.
func TestRateWindow(t *testing.T) {
	t0 := time.Unix(0, 0)
	var w rateWindow
	if got := w.observe(t0, 1000); got != -1 {
		t.Errorf("first sample rate = %d, want -1 (unknown)", got)
	}
	if got := w.observe(t0.Add(1*time.Second), 3000); got != 2000 {
		t.Errorf("rate = %d, want 2000", got)
	}
	// A poll with no byte movement must average down, not vanish.
	if got := w.observe(t0.Add(2*time.Second), 3000); got != 1000 {
		t.Errorf("rate across idle poll = %d, want 1000", got)
	}
	// Samples older than rateWindowSpan fall out of the window: a long
	// stall decays the rate all the way to 0 (never back to "unknown").
	if got := w.observe(t0.Add(3*time.Second+rateWindowSpan), 3000); got != 0 {
		t.Errorf("rate after long stall = %d, want 0", got)
	}
	// Byte regression (pull restarted) resets the window to unknown.
	if got := w.observe(t0.Add(4*time.Second+rateWindowSpan), 500); got != -1 {
		t.Errorf("rate after regression = %d, want -1 (reset)", got)
	}
}

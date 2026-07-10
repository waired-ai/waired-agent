package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/setup"
)

func pullEvents(model string) []setup.PullEvent {
	return []setup.PullEvent{
		{ModelName: model, Progress: download.Progress{State: download.StatePulling, Digest: "a", Percent: 0, Completed: 0, Total: 5_000_000_000}},
		{ModelName: model, Progress: download.Progress{State: download.StatePulling, Digest: "a", Percent: 50, Completed: 2_500_000_000, Total: 5_000_000_000, BytesPerSec: 40_000_000}},
		{ModelName: model, Progress: download.Progress{State: download.StatePulling, Digest: "a", Percent: 100, Completed: 5_000_000_000, Total: 5_000_000_000}},
		{ModelName: model, Progress: download.Progress{State: download.StateVerifying}},
		{ModelName: model, Progress: download.Progress{State: download.StateVerifying}},
		{ModelName: model, Progress: download.Progress{State: download.StateVerifying}},
		{ModelName: model, Progress: download.Progress{State: download.StateSuccess}},
	}
}

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

func TestCliPullProgressSink_NonTTY(t *testing.T) {
	var buf bytes.Buffer
	sink := cliPullProgressSink(&buf, false)
	for _, e := range pullEvents("demo") {
		sink(e)
	}
	out := buf.String()

	if !strings.Contains(out, "Downloading demo") {
		t.Errorf("missing labelled download line, got:\n%s", out)
	}
	// Aggregated byte counts + speed must render.
	if !strings.Contains(out, "2.5 GB / 5.0 GB") {
		t.Errorf("missing aggregated bytes, got:\n%s", out)
	}
	if !strings.Contains(out, "40.0 MB/s") {
		t.Errorf("missing speed, got:\n%s", out)
	}
	// Verifying must be deduped to a single line (the old sink echoed it
	// per layer event — the bug the user reported).
	if n := strings.Count(out, "Verifying"); n != 1 {
		t.Errorf("Verifying printed %d times, want 1; got:\n%s", n, out)
	}
	if !strings.Contains(out, "demo ready") {
		t.Errorf("missing completion line, got:\n%s", out)
	}
}

func TestCliPullProgressSink_TTYDedups(t *testing.T) {
	var buf bytes.Buffer
	sink := cliPullProgressSink(&buf, true)
	for _, e := range pullEvents("demo") {
		sink(e)
	}
	out := buf.String()
	if !strings.Contains(out, "Downloading demo") {
		t.Errorf("missing download line, got:\n%q", out)
	}
	if n := strings.Count(out, "Verifying"); n != 1 {
		t.Errorf("Verifying printed %d times, want 1; got:\n%q", n, out)
	}
	if !strings.Contains(out, "demo ready") {
		t.Errorf("missing completion line, got:\n%q", out)
	}
	// No leftover legacy spam.
	if strings.Contains(out, "pull:") {
		t.Errorf("legacy 'pull:' marker should be gone, got:\n%q", out)
	}
}

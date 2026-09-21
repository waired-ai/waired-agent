//go:build linux

package runtime

import (
	"bytes"
	"testing"
	"time"
)

// The start wait's rule, on a made-up clock (waired-agent#1508): wait while
// the engine writes, answers or keeps a core busy; give up after the stall
// window with none of those, or at the ceiling regardless.
func TestStartProgress(t *testing.T) {
	t0 := time.Date(2026, 9, 21, 7, 12, 38, 0, time.UTC)
	const stall, ceiling = 5 * time.Minute, 30 * time.Minute
	type reading struct {
		at    time.Duration
		out   int64
		cpu   time.Duration
		cpuOK bool
	}
	cases := []struct {
		name     string
		readings []reading
		want     startVerdict
	}{
		{"writing well past the stall window", []reading{
			{4 * time.Minute, 10, 0, false}, {8 * time.Minute, 20, 0, false}, {12 * time.Minute, 30, 0, false},
		}, startWaiting},
		{"nothing moves for the stall window", []reading{
			{time.Minute, 0, 0, false}, {5 * time.Minute, 0, 0, false},
		}, startStalled},
		{"output stops, then the window runs out", []reading{
			{time.Minute, 50, 0, false}, {6*time.Minute - time.Second, 50, 0, false}, {6 * time.Minute, 50, 0, false},
		}, startStalled},
		{"silent but a core busy (nvcc)", []reading{
			{0, 0, 0, true}, {4 * time.Minute, 0, 4 * time.Minute, true}, {8 * time.Minute, 0, 8 * time.Minute, true},
			{12 * time.Minute, 0, 12 * time.Minute, true},
		}, startWaiting},
		{"a trickle of CPU is not work", []reading{
			{0, 0, 0, true}, {3 * time.Minute, 0, 30 * time.Second, true}, {6 * time.Minute, 0, time.Minute, true},
		}, startStalled},
		{"a CPU count that goes down only resets the baseline", []reading{
			{0, 0, 10 * time.Minute, true}, {3 * time.Minute, 0, 2 * time.Minute, true}, {5 * time.Minute, 0, 2 * time.Minute, true},
		}, startStalled},
		{"working past the ceiling", []reading{
			{10 * time.Minute, 1, 0, false}, {20 * time.Minute, 2, 0, false}, {30 * time.Minute, 3, 0, false},
		}, startTimedOut},
		{"an unreadable CPU count is not work", []reading{
			{0, 0, 0, false}, {5 * time.Minute, 0, 5 * time.Minute, false},
		}, startStalled},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newStartProgress(t0, stall, ceiling)
			got := startWaiting
			for _, r := range c.readings {
				got = p.observe(t0.Add(r.at), r.out, r.cpu, r.cpuOK)
			}
			if got != c.want {
				t.Errorf("verdict = %d, want %d", got, c.want)
			}
		})
	}

	t.Run("an answered probe is work", func(t *testing.T) {
		p := newStartProgress(t0, stall, ceiling)
		p.answered(t0.Add(4 * time.Minute))
		if got := p.observe(t0.Add(8*time.Minute), 0, 0, false); got != startWaiting {
			t.Errorf("verdict = %d, want waiting: the engine answered 4 minutes ago", got)
		}
	})

	t.Run("notes at 2 minutes, then every 5", func(t *testing.T) {
		p := newStartProgress(t0, stall, ceiling)
		var at []time.Duration
		for m := time.Duration(0); m <= 13*time.Minute; m += 30 * time.Second {
			if p.dueNote(t0.Add(m)) {
				at = append(at, m)
			}
		}
		want := []time.Duration{2 * time.Minute, 7 * time.Minute, 12 * time.Minute}
		if len(at) != len(want) {
			t.Fatalf("notes at %v, want %v", at, want)
		}
		for i := range want {
			if at[i] != want[i] {
				t.Fatalf("notes at %v, want %v", at, want)
			}
		}
	})
}

// Bytes are counted before the cap drops them, so an engine whose log is
// already full still reads as writing.
func TestCountingWriter_CountsWhatTheCapDrops(t *testing.T) {
	var file bytes.Buffer
	capped := &cappedWriter{w: &file, max: 4, written: 4}
	c := &countingWriter{w: capped}
	if _, err := c.Write([]byte("compiling kernel 12/40\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if c.n.Load() != int64(len("compiling kernel 12/40\n")) {
		t.Errorf("counted %d bytes, want every byte handed in", c.n.Load())
	}
}

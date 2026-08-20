package main

import (
	"testing"
	"time"
)

// TestNetworkMapAdvanceBackoff pins the ladder and, more importantly, the
// reset. Before waired#1218 the backoff was declared outside the
// reconnect loop and never reset, so every agent settled permanently at
// the 5s ceiling — a fixed wait, applied to streams that end on a
// schedule, which is exactly the shape that keeps a fleet in lockstep.
func TestNetworkMapAdvanceBackoff(t *testing.T) {
	cases := []struct {
		name     string
		lived    time.Duration
		backoff  time.Duration
		wantWait time.Duration
		wantNext time.Duration
	}{
		{
			name:     "first short stream waits the start value",
			lived:    time.Second,
			backoff:  networkMapBackoffStart,
			wantWait: networkMapBackoffStart,
			wantNext: 2 * time.Second,
		},
		{
			name:     "repeated short streams climb",
			lived:    time.Second,
			backoff:  2 * time.Second,
			wantWait: 2 * time.Second,
			wantNext: 4 * time.Second,
		},
		{
			name:     "the ladder is capped",
			lived:    time.Second,
			backoff:  4 * time.Second,
			wantWait: 4 * time.Second,
			wantNext: networkMapBackoffMax,
		},
		{
			name:     "at the cap it stays there",
			lived:    time.Second,
			backoff:  networkMapBackoffMax,
			wantWait: networkMapBackoffMax,
			wantNext: networkMapBackoffMax,
		},
		{
			name:     "a healthy stream resets, even from the cap",
			lived:    networkMapHealthyStream,
			backoff:  networkMapBackoffMax,
			wantWait: networkMapBackoffStart,
			wantNext: 2 * time.Second,
		},
		{
			name:     "a long stream resets",
			lived:    10 * time.Minute,
			backoff:  networkMapBackoffMax,
			wantWait: networkMapBackoffStart,
			wantNext: 2 * time.Second,
		},
		{
			name:     "just under healthy does not reset",
			lived:    networkMapHealthyStream - time.Millisecond,
			backoff:  networkMapBackoffMax,
			wantWait: networkMapBackoffMax,
			wantNext: networkMapBackoffMax,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wait, next := networkMapAdvanceBackoff(tc.lived, tc.backoff)
			if wait != tc.wantWait || next != tc.wantNext {
				t.Fatalf("networkMapAdvanceBackoff(%v, %v) = (%v, %v), want (%v, %v)",
					tc.lived, tc.backoff, wait, next, tc.wantWait, tc.wantNext)
			}
		})
	}
}

// TestNetworkMapReconnectWait covers the jitter: bounded, and actually
// varying. A wait that is bounded but constant would pass a range check
// and reintroduce the lockstep the jitter exists to break.
func TestNetworkMapReconnectWait(t *testing.T) {
	const backoff = 5 * time.Second
	lo := time.Duration(float64(backoff) * 0.8)
	hi := time.Duration(float64(backoff) * 1.2)

	distinct := make(map[time.Duration]struct{})
	for range 500 {
		got := networkMapReconnectWait(backoff)
		if got < lo || got > hi {
			t.Fatalf("networkMapReconnectWait(%v) = %v, outside [%v, %v]", backoff, got, lo, hi)
		}
		distinct[got] = struct{}{}
	}
	if len(distinct) < 100 {
		t.Fatalf("only %d distinct waits in 500 draws; the source is not varying", len(distinct))
	}
}

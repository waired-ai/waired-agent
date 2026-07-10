package main

import (
	"testing"
	"time"
)

func TestSummarizeConnectivity(t *testing.T) {
	cases := []struct {
		name          string
		snap          map[string]PathSnapshot
		direct, relay int
		total         int
	}{
		{"empty", nil, 0, 0, 0},
		{
			"all direct",
			map[string]PathSnapshot{
				"a": {CurrentPath: pathDirect},
				"b": {CurrentPath: pathDirect},
			},
			2, 0, 2,
		},
		{
			"all relay",
			map[string]PathSnapshot{
				"a": {CurrentPath: pathRelay},
			},
			0, 1, 1,
		},
		{
			"mixed",
			map[string]PathSnapshot{
				"a": {CurrentPath: pathDirect},
				"b": {CurrentPath: pathRelay},
				"c": {CurrentPath: pathDirect},
			},
			2, 1, 3,
		},
		{
			"unestablished peer counts toward total only",
			map[string]PathSnapshot{
				"a": {CurrentPath: pathDirect},
				"b": {CurrentPath: ""},
			},
			1, 0, 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeConnectivity(tc.snap)
			if got.DirectPeers != tc.direct || got.RelayPeers != tc.relay || got.TotalPeers != tc.total {
				t.Fatalf("counts: want d=%d r=%d t=%d, got d=%d r=%d t=%d",
					tc.direct, tc.relay, tc.total, got.DirectPeers, got.RelayPeers, got.TotalPeers)
			}
			// Invariant the CP validator enforces.
			if got.DirectPeers+got.RelayPeers > got.TotalPeers {
				t.Fatalf("direct+relay (%d) exceeds total (%d)", got.DirectPeers+got.RelayPeers, got.TotalPeers)
			}
			if _, err := time.Parse(time.RFC3339Nano, got.LastCheck); err != nil {
				t.Fatalf("last_check not RFC3339Nano: %q (%v)", got.LastCheck, err)
			}
		})
	}
}

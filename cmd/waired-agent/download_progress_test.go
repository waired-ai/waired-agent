package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/download"
)

func TestDownloadProgress_AggregatesLayersByDigest(t *testing.T) {
	d := newDownloadProgress()

	// Two distinct layers → overall = sum of both.
	d.observe("m", download.Progress{Digest: "a", Completed: 1_000_000_000, Total: 2_000_000_000})
	d.observe("m", download.Progress{Digest: "b", Completed: 500_000_000, Total: 3_000_000_000})

	completed, total, _, ok := d.aggregate("m")
	if !ok {
		t.Fatal("aggregate ok=false, want true")
	}
	if completed != 1_500_000_000 || total != 5_000_000_000 {
		t.Fatalf("aggregate = %d / %d, want 1500000000 / 5000000000", completed, total)
	}
}

func TestDownloadProgress_SameDigestReplacesNotAdds(t *testing.T) {
	d := newDownloadProgress()
	d.observe("m", download.Progress{Digest: "a", Completed: 1_000_000_000, Total: 5_000_000_000})
	// Later update for the SAME layer must replace, not double-count.
	d.observe("m", download.Progress{Digest: "a", Completed: 4_000_000_000, Total: 5_000_000_000})

	completed, total, _, ok := d.aggregate("m")
	if !ok || completed != 4_000_000_000 || total != 5_000_000_000 {
		t.Fatalf("aggregate = %d / %d ok=%v, want 4000000000 / 5000000000 true", completed, total, ok)
	}
}

func TestDownloadProgress_IgnoresNonLayerAndSizeless(t *testing.T) {
	d := newDownloadProgress()
	d.observe("m", download.Progress{Digest: "", Completed: 9, Total: 9}) // non-layer line
	d.observe("m", download.Progress{Digest: "a", Total: 0})              // size unknown
	if _, _, _, ok := d.aggregate("m"); ok {
		t.Fatal("aggregate ok=true after only non-layer/size-less updates, want false")
	}
}

func TestDownloadProgress_UnknownModelAndForget(t *testing.T) {
	d := newDownloadProgress()
	if _, _, _, ok := d.aggregate("never"); ok {
		t.Fatal("aggregate for unknown model ok=true, want false")
	}
	d.observe("m", download.Progress{Digest: "a", Completed: 1, Total: 2})
	d.forget("m")
	if _, _, _, ok := d.aggregate("m"); ok {
		t.Fatal("aggregate after forget ok=true, want false")
	}
}

func TestDownloadProgress_NilSafe(t *testing.T) {
	var d *downloadProgress
	// None of these must panic on a nil tracker.
	d.observe("m", download.Progress{Digest: "a", Completed: 1, Total: 2})
	d.forget("m")
	if _, _, _, ok := d.aggregate("m"); ok {
		t.Fatal("nil aggregate ok=true, want false")
	}
}

// waired#1286: the model download carries its transfer rate, like the
// engine download always has. The model is the one the operator waits on
// — tens of GB against the engine's one — so it was the row with no
// "(xx MB/s)" and the longest wait.
func TestDownloadProgress_SumsTheRateOfLayersStillMoving(t *testing.T) {
	d := newDownloadProgress()
	d.observe("m", download.Progress{Digest: "a", Completed: 1_000_000_000, Total: 2_000_000_000, BytesPerSec: 40_000_000})
	d.observe("m", download.Progress{Digest: "b", Completed: 500_000_000, Total: 3_000_000_000, BytesPerSec: 10_000_000})

	if _, _, rate, ok := d.aggregate("m"); !ok || rate != 50_000_000 {
		t.Fatalf("aggregate rate = %d ok=%v, want 50000000 true", rate, ok)
	}
}

// A finished layer keeps the last speed it reported. Counting those back
// in would make a download read faster the more of it was already done —
// by the last layer the figure would be mostly the memory of transfers
// that stopped minutes ago.
func TestDownloadProgress_DropsTheRateOfFinishedLayers(t *testing.T) {
	d := newDownloadProgress()
	d.observe("m", download.Progress{Digest: "a", Completed: 2_000_000_000, Total: 2_000_000_000, BytesPerSec: 40_000_000})
	d.observe("m", download.Progress{Digest: "b", Completed: 500_000_000, Total: 3_000_000_000, BytesPerSec: 10_000_000})

	if _, _, rate, _ := d.aggregate("m"); rate != 10_000_000 {
		t.Fatalf("aggregate rate = %d, want 10000000 (only the layer still moving)", rate)
	}

	// Everything done: nothing is known to be moving, which is the same
	// thing the wire and the console mean by 0. A stall is the byte
	// counters not advancing, never a zero rate.
	d.observe("m", download.Progress{Digest: "b", Completed: 3_000_000_000, Total: 3_000_000_000, BytesPerSec: 10_000_000})
	if _, _, rate, ok := d.aggregate("m"); !ok || rate != 0 {
		t.Fatalf("aggregate rate = %d ok=%v, want 0 true", rate, ok)
	}
}

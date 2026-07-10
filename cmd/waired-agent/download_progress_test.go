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

	completed, total, ok := d.aggregate("m")
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

	completed, total, ok := d.aggregate("m")
	if !ok || completed != 4_000_000_000 || total != 5_000_000_000 {
		t.Fatalf("aggregate = %d / %d ok=%v, want 4000000000 / 5000000000 true", completed, total, ok)
	}
}

func TestDownloadProgress_IgnoresNonLayerAndSizeless(t *testing.T) {
	d := newDownloadProgress()
	d.observe("m", download.Progress{Digest: "", Completed: 9, Total: 9}) // non-layer line
	d.observe("m", download.Progress{Digest: "a", Total: 0})              // size unknown
	if _, _, ok := d.aggregate("m"); ok {
		t.Fatal("aggregate ok=true after only non-layer/size-less updates, want false")
	}
}

func TestDownloadProgress_UnknownModelAndForget(t *testing.T) {
	d := newDownloadProgress()
	if _, _, ok := d.aggregate("never"); ok {
		t.Fatal("aggregate for unknown model ok=true, want false")
	}
	d.observe("m", download.Progress{Digest: "a", Completed: 1, Total: 2})
	d.forget("m")
	if _, _, ok := d.aggregate("m"); ok {
		t.Fatal("aggregate after forget ok=true, want false")
	}
}

func TestDownloadProgress_NilSafe(t *testing.T) {
	var d *downloadProgress
	// None of these must panic on a nil tracker.
	d.observe("m", download.Progress{Digest: "a", Completed: 1, Total: 2})
	d.forget("m")
	if _, _, ok := d.aggregate("m"); ok {
		t.Fatal("nil aggregate ok=true, want false")
	}
}

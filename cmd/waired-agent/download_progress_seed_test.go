package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/download"
)

// TestDownloadProgress_SeededTotalDoesNotClimbAsLayersAppear reproduces
// the shape waired-agent#1299 reports and pins the fix.
//
// PRODUCT CONTRACT (owner ruling 2026-09-12: the bar is one bar). The
// figures are a real tag read from the registry on 2026-09-12
// (frob/qwen3.8-flash-next:125b-a6b-ud-q2_K_XL): a 0.908 GB projector
// layer, which ollama reaches first, then 78.869 GB of weights. Without a
// seed the bar fills against 0.9 GB and restarts against 79.8 GB.
func TestDownloadProgress_SeededTotalDoesNotClimbAsLayersAppear(t *testing.T) {
	const (
		projector = int64(908_000_000)
		weights   = int64(78_869_000_000)
		whole     = projector + weights
	)

	t.Run("without a seed the total climbs", func(t *testing.T) {
		d := newDownloadProgress()
		d.observe("m", download.Progress{Digest: "proj", Completed: projector, Total: projector})
		_, total, _, _ := d.aggregate("m")
		if total != projector {
			t.Fatalf("total before the weights layer = %d, want %d", total, projector)
		}
		d.observe("m", download.Progress{Digest: "w", Completed: 0, Total: weights})
		_, total, _, _ = d.aggregate("m")
		if total != whole {
			t.Fatalf("total after the weights layer = %d, want %d", total, whole)
		}
	})

	t.Run("with a seed the total is whole from the first frame", func(t *testing.T) {
		d := newDownloadProgress()
		d.seedTotal("m", whole)

		d.observe("m", download.Progress{Digest: "proj", Completed: 100_000_000, Total: projector})
		completed, total, _, ok := d.aggregate("m")
		if !ok {
			t.Fatal("aggregate ok=false, want true")
		}
		if total != whole {
			t.Errorf("total = %d, want %d", total, whole)
		}
		if completed != 100_000_000 {
			t.Errorf("completed = %d, want 100000000", completed)
		}

		// The weights layer beginning must not move the total at all.
		d.observe("m", download.Progress{Digest: "proj", Completed: projector, Total: projector})
		d.observe("m", download.Progress{Digest: "w", Completed: 0, Total: weights})
		if _, total, _, _ = d.aggregate("m"); total != whole {
			t.Errorf("total once the weights layer began = %d, want %d", total, whole)
		}

		// And the bar still reaches the end: ollama reports a blob it
		// already has as completed in one step, so completed catches up
		// to a seeded total even on a pull that fetches nothing new
		// (upstream server/download.go, the cache-hit arm).
		d.observe("m", download.Progress{Digest: "w", Completed: weights, Total: weights})
		completed, total, _, _ = d.aggregate("m")
		if completed != whole || total != whole {
			t.Errorf("finished = %d / %d, want %d / %d", completed, total, whole, whole)
		}
	})
}

// TestDownloadProgress_SeedFallsBackAndNeverShrinksTheTruth covers what a
// caller that could not reach the registry, or reached a manifest that
// disagrees with the pull, gets.
func TestDownloadProgress_SeedFallsBackAndNeverShrinksTheTruth(t *testing.T) {
	t.Run("no seed keeps the summed total", func(t *testing.T) {
		d := newDownloadProgress()
		d.observe("m", download.Progress{Digest: "a", Completed: 1, Total: 500})
		if _, total, _, _ := d.aggregate("m"); total != 500 {
			t.Errorf("total = %d, want 500", total)
		}
	})

	t.Run("a non-positive seed is not a total", func(t *testing.T) {
		d := newDownloadProgress()
		d.seedTotal("m", 0)
		d.seedTotal("m", -1)
		d.observe("m", download.Progress{Digest: "a", Completed: 1, Total: 500})
		if _, total, _, _ := d.aggregate("m"); total != 500 {
			t.Errorf("total = %d, want 500", total)
		}
	})

	t.Run("layers larger than the seed win", func(t *testing.T) {
		d := newDownloadProgress()
		d.seedTotal("m", 400)
		d.observe("m", download.Progress{Digest: "a", Completed: 500, Total: 900})
		completed, total, _, _ := d.aggregate("m")
		if total != 900 {
			t.Errorf("total = %d, want 900 — what is being fetched outranks the manifest", total)
		}
		if completed != 500 {
			t.Errorf("completed = %d, want 500", completed)
		}
	})

	t.Run("completed is clamped to the total", func(t *testing.T) {
		d := newDownloadProgress()
		d.seedTotal("m", 1000)
		// A layer that reports more done than its own total: the sum must
		// not hand a caller a percentage above 100.
		d.observe("m", download.Progress{Digest: "a", Completed: 1500, Total: 1000})
		completed, total, _, _ := d.aggregate("m")
		if completed != total {
			t.Errorf("completed/total = %d/%d, want them equal", completed, total)
		}
	})

	t.Run("forget drops the seed with the layers", func(t *testing.T) {
		d := newDownloadProgress()
		d.seedTotal("m", 1000)
		d.observe("m", download.Progress{Digest: "a", Completed: 10, Total: 20})
		d.forget("m")
		if _, _, _, ok := d.aggregate("m"); ok {
			t.Error("aggregate ok=true after forget, want false")
		}
		// A seed left behind would give the next pull of the same model a
		// total it never asked for.
		d.observe("m", download.Progress{Digest: "b", Completed: 1, Total: 7})
		if _, total, _, _ := d.aggregate("m"); total != 7 {
			t.Errorf("total on the next pull = %d, want 7", total)
		}
	})

	t.Run("a nil tracker still takes a seed", func(t *testing.T) {
		var d *downloadProgress
		d.seedTotal("m", 1000) // must not panic
	})
}

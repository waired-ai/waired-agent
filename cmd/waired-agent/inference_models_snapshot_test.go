package main

import (
	"slices"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// noProgress is the aggregator for a host with nothing in flight.
func noProgress(string) (int64, int64, bool) { return 0, 0, false }

// TestModelsSnapshotCarriesTheFailureReason is waired-agent#328's
// regression bar on the daemon side. Product contract: a failed model's
// stored reason reaches the local management API.
//
// The rc7 host's journal held `ollama pull failed … err="exit status 1"`
// and `err="download: start ollama: context canceled"` the whole time,
// and every reader downstream — `waired models pull`, `waired runtimes
// benchmark`, the wizard — got a bare name because this projection
// dropped it.
func TestModelsSnapshotCarriesTheFailureReason(t *testing.T) {
	const reason = "download: start ollama: context canceled"
	snap := modelsSnapshot(map[string]catalog.ModelState{
		"failed-model": {State: catalog.ModelStateFailed, Error: reason},
	}, noProgress)

	if !slices.Contains(snap.Failed, "failed-model") {
		t.Fatalf("Failed = %v, want the model named", snap.Failed)
	}
	if len(snap.Failures) != 1 {
		t.Fatalf("Failures = %+v, want exactly one entry", snap.Failures)
	}
	if snap.Failures[0].Model != "failed-model" || snap.Failures[0].Error != reason {
		t.Fatalf("Failures[0] = %+v, want the model and its stored reason", snap.Failures[0])
	}
}

// TestModelsSnapshotOmitsAnUnrecordedReason: a failure with no stored
// text must not gain one. Old clients read Failed alone and new ones
// degrade to it, so an absent entry is a real answer — a fabricated one
// would be a guess presented as the daemon's own words.
func TestModelsSnapshotOmitsAnUnrecordedReason(t *testing.T) {
	snap := modelsSnapshot(map[string]catalog.ModelState{
		"failed-model": {State: catalog.ModelStateFailed},
	}, noProgress)

	if !slices.Contains(snap.Failed, "failed-model") {
		t.Fatalf("Failed = %v, want the model named anyway", snap.Failed)
	}
	if len(snap.Failures) != 0 {
		t.Fatalf("Failures = %+v, want none", snap.Failures)
	}
}

// TestModelsSnapshotKeepsTheOtherLanes records that adding the failure
// reason changed nothing about the three lists that were already there,
// including the byte progress that only in-flight downloads carry.
func TestModelsSnapshotKeepsTheOtherLanes(t *testing.T) {
	snap := modelsSnapshot(map[string]catalog.ModelState{
		"ready-model":   {State: catalog.ModelStateReady},
		"pulling-model": {State: catalog.ModelStateDownloading},
		"queued-model":  {State: catalog.ModelStateQueued},
		"gone-model":    {State: catalog.ModelStateNotPresent},
		"failed-model":  {State: catalog.ModelStateFailed, Error: "no space left on device"},
	}, func(id string) (int64, int64, bool) {
		if id == "pulling-model" {
			return 1_500_000_000, 4_300_000_000, true
		}
		return 0, 0, false
	})

	if !slices.Equal(snap.Ready, []string{"ready-model"}) {
		t.Errorf("Ready = %v", snap.Ready)
	}
	// Map iteration order is not stable, so compare as a set.
	slices.Sort(snap.Downloading)
	if !slices.Equal(snap.Downloading, []string{"pulling-model", "queued-model"}) {
		t.Errorf("Downloading = %v", snap.Downloading)
	}
	if len(snap.Downloads) != 1 || snap.Downloads[0].Model != "pulling-model" ||
		snap.Downloads[0].TotalBytes != 4_300_000_000 {
		t.Errorf("Downloads = %+v, want only the model with bytes in flight", snap.Downloads)
	}
	// not_present is on none of the lists — the state exists precisely to
	// mean "nothing to say about this model".
	if slices.Contains(snap.Ready, "gone-model") || slices.Contains(snap.Downloading, "gone-model") ||
		slices.Contains(snap.Failed, "gone-model") {
		t.Errorf("not_present leaked onto a list: %+v", snap)
	}
}

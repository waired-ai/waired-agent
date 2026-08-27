package main

import (
	"slices"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// noProgress is the aggregator for a host with nothing in flight.
func noProgress(string) (int64, int64, int64, bool) { return 0, 0, 0, false }

// snapshotCatalog is the manifest set these tests project against. Only
// the ids matter here: modelsSnapshot reads nothing else off a manifest.
func snapshotCatalog(ids ...string) []catalog.Manifest {
	out := make([]catalog.Manifest, 0, len(ids))
	for _, id := range ids {
		out = append(out, catalog.Manifest{ModelID: id})
	}
	return out
}

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
	}, snapshotCatalog("failed-model"), noProgress)

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
	}, snapshotCatalog("failed-model"), noProgress)

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
//
// The not_present assert at the bottom is INVERTED as of waired-agent#403:
// it used to say the state appears on no list at all. That was a record of
// today's behaviour, and #403 is the case for changing it — "no list"
// could not be told apart from "no such model".
func TestModelsSnapshotKeepsTheOtherLanes(t *testing.T) {
	snap := modelsSnapshot(map[string]catalog.ModelState{
		"ready-model":   {State: catalog.ModelStateReady},
		"pulling-model": {State: catalog.ModelStateDownloading},
		"queued-model":  {State: catalog.ModelStateQueued},
		"gone-model":    {State: catalog.ModelStateNotPresent},
		"failed-model":  {State: catalog.ModelStateFailed, Error: "no space left on device"},
	}, snapshotCatalog("ready-model", "pulling-model", "queued-model", "gone-model", "failed-model"),
		func(id string) (int64, int64, int64, bool) {
			if id == "pulling-model" {
				return 1_500_000_000, 4_300_000_000, 40_000_000, true
			}
			return 0, 0, 0, false
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
	if slices.Contains(snap.Ready, "gone-model") || slices.Contains(snap.Downloading, "gone-model") ||
		slices.Contains(snap.Failed, "gone-model") {
		t.Errorf("not_present leaked onto a working list: %+v", snap)
	}
	if !slices.Equal(snap.NotPresent, []string{"gone-model"}) {
		t.Errorf("NotPresent = %v, want the not_present model and nothing else", snap.NotPresent)
	}
}

// TestModelsSnapshotNamesAModelNothingHasStartedOn is waired-agent#403's
// regression bar. Product contract (#403): /inference/status can express
// "this model is in the catalog and nothing has started on it".
//
// The state map is the catalog CACHE, so this model — the common case, a
// host that has downloaded one model out of the catalog's twenty — has no
// entry to sort at all. Before #403 the only observation available was
// "on none of the lists", which is what `waired init` had to bound with a
// blind five-minute grace and what `waired models pull --wait` printed
// nothing for.
func TestModelsSnapshotNamesAModelNothingHasStartedOn(t *testing.T) {
	snap := modelsSnapshot(map[string]catalog.ModelState{
		"served-model": {State: catalog.ModelStateReady},
	}, snapshotCatalog("served-model", "untouched-model"), noProgress)

	if !slices.Equal(snap.NotPresent, []string{"untouched-model"}) {
		t.Fatalf("NotPresent = %v, want the model with no state row at all", snap.NotPresent)
	}
	if !slices.Equal(snap.Ready, []string{"served-model"}) {
		t.Errorf("Ready = %v, want the served model untouched by this change", snap.Ready)
	}
}

// An evicted model reports as not present. It is one of the two states
// #403 names as falling through the switch silently, and for the question
// this list answers — is anything under way — its history does not
// matter: the weights are not on disk and nothing is fetching them.
func TestModelsSnapshotCountsEvictedAsNotPresent(t *testing.T) {
	snap := modelsSnapshot(map[string]catalog.ModelState{
		"evicted-model": {State: catalog.ModelStateEvicted},
	}, snapshotCatalog("evicted-model"), noProgress)

	if !slices.Equal(snap.NotPresent, []string{"evicted-model"}) {
		t.Fatalf("NotPresent = %v, want the evicted model", snap.NotPresent)
	}
}

// A state row for an id this build's catalog does not carry still reaches
// its lane, and does not turn up under not_present. The two halves of the
// projection have different sources on purpose — the lanes report what the
// daemon HAS DONE, not-present reports what it COULD do — and a model
// dropped from the catalog while its weights are still on disk is exactly
// where conflating them would lose the row.
func TestModelsSnapshotKeepsAStateRowOutsideTheCatalog(t *testing.T) {
	snap := modelsSnapshot(map[string]catalog.ModelState{
		"retired-model": {State: catalog.ModelStateReady},
	}, snapshotCatalog("shipped-model"), noProgress)

	if !slices.Equal(snap.Ready, []string{"retired-model"}) {
		t.Errorf("Ready = %v, want the on-disk model reported anyway", snap.Ready)
	}
	if !slices.Equal(snap.NotPresent, []string{"shipped-model"}) {
		t.Errorf("NotPresent = %v, want only the catalog model nothing has started on", snap.NotPresent)
	}
}

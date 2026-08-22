package main

import (
	"context"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/management"
)

// The hold (waired-ai/waired#1247): the wizard now asks about the speed
// check on the model step and writes the answer WITH the choice, so the
// generation counter is bumped minutes before there is anything to
// measure. Spending the request there is permanent — RunBootBenchmark
// answers `skipped` with no engine port, `skipped` is a recorded ending,
// and a recorded ending at the requested generation satisfies the re-run
// guard forever.
func TestSetupBenchmarkHeldUntilTheModelIsServed(t *testing.T) {
	cases := []struct {
		name        string
		desiredID   string
		activeModel string
		modelState  string
		wantStart   bool
		because     string
	}{
		{
			name: "no model has been asked for", desiredID: "",
			wantStart: true,
			because:   "the request is about whatever this host already serves, so there is nothing to wait for",
		},
		{
			name: "the weights have not arrived", desiredID: "m1",
			activeModel: "m1", modelState: catalog.ModelStateDownloading,
			wantStart: false,
			because:   "there is nothing on disk to measure",
		},
		{
			name: "the weights are there but the host still serves the old model", desiredID: "m1",
			activeModel: "m0", modelState: catalog.ModelStateReady,
			wantStart: false,
			because:   "the benchmark measures the ACTIVE selection, so it would time the model being replaced",
		},
		{
			name: "downloaded and serving", desiredID: "m1",
			activeModel: "m1", modelState: catalog.ModelStateReady,
			wantStart: true,
			because:   "there is something to measure and it is the thing that was asked about",
		},
		{
			name: "serving it, but its weights are being re-pulled", desiredID: "m1",
			activeModel: "m1", modelState: catalog.ModelStateDownloading,
			wantStart: false,
			because:   "a selection outlives readiness — re-pulling the active model moves its state back without clearing it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSetupProvider{modelState: tc.modelState, activeModel: tc.activeModel}
			r := watchingReconciler(f, nil, "dev-1", nil, quietLogger())
			r.Apply(context.Background(), desiredFrame("", tc.desiredID, 3))

			started := len(f.benchStarts) > 0
			if started != tc.wantStart {
				t.Fatalf("benchStarts = %v, want started=%v — %s", f.benchStarts, tc.wantStart, tc.because)
			}
		})
	}
}

// The hold is not a refusal: the run starts the moment the wait ends.
//
// And it must start from the REPORTER's tick, not from a network-map
// frame. Apply runs only when a frame arrives and the edge this waits for
// — a download finishing — moves no map epoch, so a request left on
// frames alone would have no reader at the moment it became runnable.
func TestSetupBenchmarkStartsWhenTheDownloadFinishes(t *testing.T) {
	f := &fakeSetupProvider{modelState: catalog.ModelStateDownloading, activeModel: "m1"}
	r := watchingReconciler(f, nil, "dev-1", nil, quietLogger())
	r.Apply(context.Background(), desiredFrame("", "m1", 3))
	if len(f.benchStarts) != 0 {
		t.Fatalf("benchStarts = %v, want none while the download runs", f.benchStarts)
	}

	// The download finishes. No frame arrives — only the reporter ticks.
	f.mu.Lock()
	f.modelState = catalog.ModelStateReady
	f.mu.Unlock()
	r.reconcileBenchmark()

	if len(f.benchStarts) != 1 || f.benchStarts[0] != 3 {
		t.Fatalf("benchStarts = %v, want one start at gen 3 from the reporter's own tick", f.benchStarts)
	}

	// And only one: the fake reports the job as running, which is the
	// same guard Apply uses.
	f.mu.Lock()
	f.bench = management.BenchmarkStatusResponse{State: management.BenchmarkStateRunning}
	f.mu.Unlock()
	r.reconcileBenchmark()
	if len(f.benchStarts) != 1 {
		t.Fatalf("benchStarts = %v, want the tick not to start a second run", f.benchStarts)
	}
}

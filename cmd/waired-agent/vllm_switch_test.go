package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// The whole rule for how a vLLM host reaches the chosen model
// (waired-agent#1515). Product contract: owner decision 2026-09-21, a vLLM
// host keeps answering with the model it was running until the chosen one's
// weights are on disk, and switches then.
func TestPlanVLLMTarget(t *testing.T) {
	base := vllmTargetFacts{TargetModel: "new", TargetVariant: "bf16", AllowPull: true}
	with := func(mut func(*vllmTargetFacts)) vllmTargetFacts {
		f := base
		mut(&f)
		return f
	}
	for _, tc := range []struct {
		name  string
		facts vllmTargetFacts
		want  vllmTargetPlan
	}{
		{"on disk, already serving it", with(func(f *vllmTargetFacts) {
			f.TargetOnDisk, f.EngineUp, f.ServingModel, f.ServingVariant = true, true, "new", "bf16"
		}), vllmTargetPlan{Action: vllmKeep}},
		{"on disk, serving another build of it", with(func(f *vllmTargetFacts) {
			f.TargetOnDisk, f.EngineUp, f.ServingModel, f.ServingVariant = true, true, "new", "fp8"
		}), vllmTargetPlan{Action: vllmSwitchToTarget}},
		{"on disk, serving another model", with(func(f *vllmTargetFacts) {
			f.TargetOnDisk, f.EngineUp, f.ServingModel = true, true, "old"
		}), vllmTargetPlan{Action: vllmSwitchToTarget}},
		{"on disk, nothing up", with(func(f *vllmTargetFacts) {
			f.TargetOnDisk = true
		}), vllmTargetPlan{Action: vllmStartTarget}},
		{"absent, the old model keeps answering while it downloads", with(func(f *vllmTargetFacts) {
			f.EngineUp, f.ServingModel = true, "old"
		}), vllmTargetPlan{Action: vllmKeep, Download: true}},
		{"absent, nothing up, the previous model answers meanwhile", with(func(f *vllmTargetFacts) {
			f.HasPrevious = true
		}), vllmTargetPlan{Action: vllmStartPrevious, Download: true}},
		// The fleet host of #1515: the previous model was retired, so
		// nothing answers until the download lands.
		{"absent, nothing up, nothing to run meanwhile", base,
			vllmTargetPlan{Action: vllmWait, Download: true}},
		{"absent and already downloading", with(func(f *vllmTargetFacts) {
			f.TargetDownloading = true
		}), vllmTargetPlan{Action: vllmWait}},
		// A cancelled download is not restarted by the next trigger.
		{"absent, and this process already started its download", with(func(f *vllmTargetFacts) {
			f.AlreadyDispatched, f.EngineUp = true, true
		}), vllmTargetPlan{Action: vllmKeep}},
		{"absent, downloads off, nothing to run", with(func(f *vllmTargetFacts) {
			f.AllowPull = false
		}), vllmTargetPlan{Action: vllmRefuseNoPull}},
		{"absent, downloads off, the previous model answers", with(func(f *vllmTargetFacts) {
			f.AllowPull, f.HasPrevious = false, true
		}), vllmTargetPlan{Action: vllmStartPrevious}},
		{"absent, downloads off, the old model keeps answering", with(func(f *vllmTargetFacts) {
			f.AllowPull, f.EngineUp = false, true
		}), vllmTargetPlan{Action: vllmKeep}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planVLLMTarget(tc.facts); got != tc.want {
				t.Errorf("plan = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// Which model may answer while the chosen one downloads.
func TestVLLMPreviousCandidate(t *testing.T) {
	manifests := []catalog.Manifest{{
		ModelID: "qwen3.5-4b",
		Variants: []catalog.Variant{{
			VariantID: "bf16", RuntimeSupport: []string{catalog.RuntimeVLLM},
			Source: catalog.VariantSource{Type: catalog.SourceHuggingFace, RepoID: "Qwen/Qwen3.5-4B"},
		}},
	}}
	active := func(model, variant, runtime string) *catalog.ActiveSelection {
		return &catalog.ActiveSelection{ModelID: model, VariantID: variant, Runtime: runtime}
	}
	ready := catalog.State{VLLMModels: map[string]catalog.ModelState{
		"qwen3.5-4b":  {State: catalog.ModelStateReady, VariantID: "bf16", LocalPath: "/models/hf/Qwen__Qwen3.5-4B"},
		"gpt-oss-20b": {State: catalog.ModelStateReady, VariantID: "mxfp4-safetensors", LocalPath: "/models/hf/openai__gpt-oss-20b"},
	}}
	var asked []string
	exists := func(p string) bool { asked = append(asked, p); return true }
	var askedBuild []string
	startable := func(ok bool) func(catalog.Manifest, catalog.Variant) bool {
		return func(m catalog.Manifest, v catalog.Variant) bool {
			askedBuild = append(askedBuild, m.ModelID+"/"+v.VariantID)
			return ok
		}
	}

	for _, tc := range []struct {
		name      string
		active    *catalog.ActiveSelection
		st        catalog.State
		target    string
		exists    func(string) bool
		startable func(catalog.Manifest, catalog.Variant) bool
		want      bool
	}{
		{"the model the engine ran", active("qwen3.5-4b", "bf16", catalog.RuntimeVLLM), ready, "qwen3.6-35b-a3b", exists, startable(true), true},
		// The fleet host of #1515: gpt-oss-20b is retired, so it is not in
		// this build's catalog, and nothing may start it.
		{"a retired model", active("gpt-oss-20b", "mxfp4-safetensors", catalog.RuntimeVLLM), ready, "qwen3.6-35b-a3b", exists, startable(true), false},
		{"it is the chosen model", active("qwen3.5-4b", "bf16", catalog.RuntimeVLLM), ready, "qwen3.5-4b", exists, startable(true), false},
		{"it ran on ollama", active("qwen3.5-4b", "bf16", catalog.RuntimeOllama), ready, "qwen3.6-35b-a3b", exists, startable(true), false},
		{"another build is what is on disk", active("qwen3.5-4b", "fp8", catalog.RuntimeVLLM), ready, "qwen3.6-35b-a3b", exists, startable(true), false},
		{"its weights are not ready", active("qwen3.5-4b", "bf16", catalog.RuntimeVLLM),
			catalog.State{VLLMModels: map[string]catalog.ModelState{"qwen3.5-4b": {State: catalog.ModelStateDownloading, VariantID: "bf16", LocalPath: "/x"}}},
			"qwen3.6-35b-a3b", exists, startable(true), false},
		{"its directory is gone", active("qwen3.5-4b", "bf16", catalog.RuntimeVLLM), ready, "qwen3.6-35b-a3b",
			func(string) bool { return false }, startable(true), false},
		// The fleet host again: the model that was running was a 35B its
		// card could not hold, and starting it to fill in was minutes of
		// nothing answering.
		{"this computer cannot start it", active("qwen3.5-4b", "bf16", catalog.RuntimeVLLM), ready, "qwen3.6-35b-a3b", exists, startable(false), false},
		{"nothing recorded", nil, ready, "qwen3.6-35b-a3b", exists, startable(true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked, askedBuild = nil, nil
			_, _, path, ok := vllmPreviousCandidate(tc.active, manifests, tc.st, tc.target, "0.29.0", tc.exists, tc.startable)
			if ok != tc.want {
				t.Fatalf("eligible = %v, want %v", ok, tc.want)
			}
			if ok && (path != "/models/hf/Qwen__Qwen3.5-4B" || len(asked) != 1 || asked[0] != path) {
				t.Errorf("path = %q, asked about %v; want the weights directory, checked", path, asked)
			}
			if ok && (len(askedBuild) != 1 || askedBuild[0] != "qwen3.5-4b/bf16") {
				t.Errorf("startable asked about %v, want the previous build", askedBuild)
			}
		})
	}
}

// Which builds this computer may start when nobody chose them just now
// (waired-agent#1515). Record of today's behaviour.
func TestVLLMStartable(t *testing.T) {
	here := catalog.LoadContext{EngineKind: catalog.RuntimeVLLM, EngineVersion: "0.29.0", GPUModel: "gpu", VRAMTotalMB: 24576}
	elsewhere := here
	elsewhere.VRAMTotalMB = 49152
	m := catalog.Manifest{ModelID: "m"}
	sha := func(m catalog.Manifest, v catalog.Variant) string { return m.ModelID + "/" + v.VariantID }
	failed := func(ctx catalog.LoadContext) catalog.State {
		return catalog.State{FailedLoads: map[string]catalog.VariantLoadFailure{
			"m/big": {ModelID: "m", VariantID: "big", Context: ctx, Shape: catalog.LoadShape{ContextLength: 200704}},
		}}
	}
	for _, tc := range []struct {
		name   string
		st     catalog.State
		budget int
		v      catalog.Variant
		want   bool
	}{
		{"fits", catalog.State{}, 20889, catalog.Variant{VariantID: "small", MinVRAMMB: 12288}, true},
		{"the catalog minimum is more than vLLM may use", catalog.State{}, 20889, catalog.Variant{VariantID: "big", MinVRAMMB: 36864}, false},
		{"no minimum in the catalog", catalog.State{}, 20889, catalog.Variant{VariantID: "big"}, true},
		{"no budget known", catalog.State{}, 0, catalog.Variant{VariantID: "big", MinVRAMMB: 36864}, true},
		// In any shape: the record's shape is the chosen start's, and a
		// start to fill in would use its own.
		{"it failed to start on this computer", failed(here), 20889, catalog.Variant{VariantID: "big"}, false},
		{"it failed on another computer", failed(elsewhere), 20889, catalog.Variant{VariantID: "big"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vllmStartable(tc.st, here, tc.budget, sha)(m, tc.v); got != tc.want {
				t.Errorf("startable = %v, want %v", got, tc.want)
			}
		})
	}
}

// A start asked for while another runs is run once more afterwards instead
// of being dropped (waired-agent#1515): the weights of a vLLM switch can
// land while the previous model is still starting.
func TestRunEngineBootstrap_RunsAgainWhenAskedDuringARun(t *testing.T) {
	gate := make(chan struct{})
	var calls atomic.Int32
	p := &agentInferenceProvider{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		// The first gate every start passes; blocking it holds the first
		// run in flight, and every call is one run.
		isInferenceDisabled: func() bool {
			if calls.Add(1) == 1 {
				<-gate
			}
			return true
		},
	}
	done := make(chan struct{})
	go func() { p.runEngineBootstrap(context.Background(), "first"); close(done) }()
	waitForCond(t, 2*time.Second, "the first run to start", func() bool { return calls.Load() == 1 })
	p.runEngineBootstrap(context.Background(), "second") // returns at once: one is running
	close(gate)
	<-done
	if n := calls.Load(); n != 2 {
		t.Errorf("runs = %d, want 2: the request made during the first run was dropped", n)
	}
	if p.engineStartInFlight.Load() {
		t.Error("the start claim was left set")
	}
}

// A finished download moves Active only when no engine is up. While one is,
// it still serves the previous model, and the switch writes Active once the
// engine is ready on the new one. Product contract: owner decision
// 2026-09-21 on waired-agent#1515.
func TestHFWeightsLanded_ActiveMovesOnlyWithNothingUp(t *testing.T) {
	for _, tc := range []struct {
		name     string
		engineUp bool
		want     string
	}{
		{"an engine is up: Active stays on the model it serves", true, "previous"},
		{"nothing is up: the landed model is recorded", false, "hybrid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := vllmSwapProvider(t)
			if tc.engineUp {
				p.setVLLM(&recordingAdapter{name: "vllm", health: infruntime.StateReady})
			}
			chosen := "hybrid"
			p.preferredOverride.Store(&chosen)
			if err := p.store.Update(func(s *catalog.State) {
				s.Active = &catalog.ActiveSelection{Runtime: catalog.RuntimeVLLM, ModelID: "previous", VariantID: "bf16"}
				s.VLLMModels = map[string]catalog.ModelState{
					"hybrid": {State: catalog.ModelStateReady, VariantID: "safetensors", LocalPath: t.TempDir()},
				}
			}); err != nil {
				t.Fatal(err)
			}
			p.hfWeightsLanded(context.Background(), "hybrid", "safetensors")
			st, err := p.store.Load()
			if err != nil {
				t.Fatal(err)
			}
			if st.Active == nil || st.Active.ModelID != tc.want {
				t.Errorf("Active = %+v, want %s", st.Active, tc.want)
			}
		})
	}
}

// How a run of vLLM start attempts ends (waired-agent#1515). The moved-on
// rows are the fleet host's case: a 35B its card could not hold was being
// retried, a minute an attempt, when the next model was chosen, and the
// last failure held the engine off with the new model on disk. Record of
// today's behaviour.
func TestRunVLLMStartAttempts(t *testing.T) {
	failed := errors.New("vllm: process exited during startup")
	for _, tc := range []struct {
		name string
		// results is what each start returns, in order.
		results []error
		// movedOnAfter is the attempt after whose failure the choice has
		// moved on; 0 never.
		movedOnAfter int
		// movedOnInWait is the wait during which the choice moves on; 0
		// never.
		movedOnInWait int
		// cancelAt is the wait that finds the context ended; 0 never.
		cancelAt  int
		wantEnd   vllmAttemptsEnd
		wantCalls int
		wantWaits []int
	}{
		{"up at once", []error{nil}, 0, 0, 0, vllmAttemptsStarted, 1, nil},
		{"up at the second", []error{failed, nil}, 0, 0, 0, vllmAttemptsStarted, 2, []int{1}},
		{"every attempt fails", []error{failed, failed, failed}, 0, 0, 0, vllmAttemptsFailed, 3, []int{1, 2}},
		{"a park raced the start", []error{infruntime.ErrEngineParked}, 0, 0, 0, vllmAttemptsLatched, 1, nil},
		{"recovery gave up", []error{failed, infruntime.ErrEngineUnrecoverable}, 0, 0, 0, vllmAttemptsLatched, 2, []int{1}},
		{"moved on after the first", []error{failed, failed, failed}, 1, 0, 0, vllmAttemptsMovedOn, 1, nil},
		// The hardware case: chosen a second after the first failure.
		{"moved on during the wait", []error{failed, nil}, 0, 1, 0, vllmAttemptsMovedOn, 1, []int{1}},
		{"moved on during the last", []error{failed, failed, failed}, 3, 0, 0, vllmAttemptsMovedOn, 3, []int{1, 2}},
		{"the daemon stops between", []error{failed, failed, failed}, 0, 0, 1, vllmAttemptsCancelled, 1, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			ensure := func(context.Context) error {
				err := tc.results[calls]
				calls++
				return err
			}
			var waits []int
			movedOn := func() bool {
				return (tc.movedOnAfter != 0 && calls >= tc.movedOnAfter) ||
					(tc.movedOnInWait != 0 && len(waits) >= tc.movedOnInWait)
			}
			wait := func(_ context.Context, attempt int) bool {
				waits = append(waits, attempt)
				return attempt != tc.cancelAt
			}
			end, err := runVLLMStartAttempts(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)),
				ensure, movedOn, wait)
			if end != tc.wantEnd || calls != tc.wantCalls || !slices.Equal(waits, tc.wantWaits) {
				t.Errorf("end %d after %d starts, waits %v; want %d, %d, %v", end, calls, waits, tc.wantEnd, tc.wantCalls, tc.wantWaits)
			}
			if (end == vllmAttemptsStarted) != (err == nil) {
				t.Errorf("err = %v with end %d", err, end)
			}
		})
	}
}

// Whether the model a start was asked for is still the one chosen.
func TestVLLMChoiceMovedOn(t *testing.T) {
	p := activeReaderProvider(t)
	p.manifests = []catalog.Manifest{{ModelID: "a"}, {ModelID: "b"}}
	if p.vllmChoiceMovedOn("a") {
		t.Error("nothing chosen reads as a change")
	}
	chosen := "a"
	p.preferredOverride.Store(&chosen)
	if p.vllmChoiceMovedOn("a") {
		t.Error("the model still chosen reads as a change")
	}
	if !p.vllmChoiceMovedOn("b") {
		t.Error("a start of b, with a chosen, did not read as a change")
	}
}

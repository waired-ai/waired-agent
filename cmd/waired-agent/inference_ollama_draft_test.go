package main

import (
	"context"
	"os"
	"slices"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/platform/proclist"
	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// The draft the runner runs is recorded off its command line, and a tag
// whose runner runs another draft than this host should — none where the
// product writes one, or one where the load with it does not fit — is
// pulled again with the right one (waired-ai/waired#1433).
func TestApplyOllamaTuningVerification_RecordsAndRepairsTheDraft(t *testing.T) {
	m, base, hw, tn := verifyFixture()
	weight := int64(10e9)
	size := weight + int64(0.5*65536*float64(verifyCtx))

	stamped := base
	gg := catalog.GGUFLayout{BlockCount: 41, NextNLayers: 1}
	stamped.GGUF = &gg
	stamped.MTPDraftTokens = 2
	// Too heavy for the 24 GB fixture host with its draft: this host
	// should run none (hostfit.OllamaDraftTokens).
	heavy := stamped
	heavy.EstimatedWeightGB = 30
	publisher := stamped
	pg := gg
	pg.DraftMaxTokens = 2
	publisher.GGUF = &pg
	publisher.MTPDraftTokens = 0

	runner := func(extra ...string) func() ([]proclist.ProcInfo, error) {
		argv := append([]string{"llama-server", "-c", strconv.Itoa(verifyCtx), "-np", "1"}, extra...)
		return func() ([]proclist.ProcInfo, error) { return []proclist.ProcInfo{{PID: 20, Argv: argv}}, nil }
	}
	cases := []struct {
		name       string
		v          catalog.Variant
		procs      func() ([]proclist.ProcInfo, error)
		wantMethod string
		wantTokens int
		wantRepair string // "" = none, else "<tag>/<draft>"
	}{
		{"stamped-and-running", stamped, runner("--spec-type", "draft-mtp", "--spec-draft-n-max", "2"), "draft-mtp", 2, ""},
		{"stamped-but-not-running", stamped, runner(), "", 0, verifyTag + "/2"},
		{"stamped-but-does-not-fit", heavy, runner("--spec-type", "draft-mtp", "--spec-draft-n-max", "2"), "draft-mtp", 2, verifyTag + "/0"},
		{"does-not-fit-and-not-running", heavy, runner(), "", 0, ""},
		{"publisher-draft-not-running", publisher, runner(), "", 0, ""},
		{"no-draft-wanted", base, runner(), "", 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: size, psCtx: verifyCtx, tagSize: weight}
			srv := api.server(t)
			defer srv.Close()
			var repaired []string
			deps := ollamaVerifyDeps{ListProcs: c.procs, RestampDraft: func(tag string, _ catalog.Variant, draft int) {
				repaired = append(repaired, tag+"/"+strconv.Itoa(draft))
			}}
			sw := &fakeModelEnvSwitcher{}
			applyOllamaTuningVerification(context.Background(), sw, tn, m, c.v, hw,
				verifyTag, srv.URL, srv.Client(), deps, testLogger())
			got := sw.lastTuning(t)
			if got.SpeculativeMethod != c.wantMethod || got.SpeculativeTokens != c.wantTokens {
				t.Errorf("recorded draft %q/%d, want %q/%d", got.SpeculativeMethod, got.SpeculativeTokens, c.wantMethod, c.wantTokens)
			}
			want := []string(nil)
			if c.wantRepair != "" {
				want = []string{c.wantRepair}
			}
			if !slices.Equal(repaired, want) {
				t.Errorf("repairs = %v, want %v", repaired, want)
			}
		})
	}
}

// The repair pulls a tag at most once per draft per process: the
// verification runs on every engine start and model switch, and a runner
// that still shows the other draft after one re-pull would not change on a
// second.
func TestRestampDraft_PullsATagOnce(t *testing.T) {
	r := &draftPullRecorder{}
	p := &agentInferenceProvider{logger: testLogger()}
	p.puller = download.NewPuller("ollama-fake", r)
	v := catalog.Variant{VariantID: "mtp-q2-gguf", Renderer: "qwen3.5", Parser: "qwen3.5", MTPDraftTokens: 2}
	for range 3 {
		p.restampDraft("hf.co/ns/m:q2", v, 2)
	}
	p.restampDraft("hf.co/ns/m:q3", v, 2)
	p.pullsWG.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	pulls := 0
	for _, args := range r.calls {
		if args[0] == "pull" {
			pulls++
		}
	}
	if pulls != 2 {
		t.Errorf("commands = %v, want one pull per tag (2)", r.calls)
	}
	sort.Strings(r.modelfiles)
	if len(r.modelfiles) != 2 || r.modelfiles[0] != "FROM hf.co/ns/m:q2\nRENDERER qwen3.5\nPARSER qwen3.5\nPARAMETER draft_num_predict 2\n" {
		t.Errorf("modelfiles = %q, want each pull followed by a write of the renderer and the draft", r.modelfiles)
	}
}

type draftPullRecorder struct {
	mu         sync.Mutex
	calls      [][]string
	modelfiles []string
}

func (r *draftPullRecorder) Run(_ context.Context, _ string, args, _ []string, _ func(string)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, args)
	if args[0] == "create" {
		b, err := os.ReadFile(args[3])
		if err != nil {
			return err
		}
		r.modelfiles = append(r.modelfiles, string(b))
	}
	return nil
}

// The draft Pull writes is the one this host should run: the catalog's
// draft where the load with it fits the device, none where it does not
// (hostfit.OllamaDraftTokens, waired-ai/waired#1433).
func TestOllamaDraftToWrite_FollowsTheHost(t *testing.T) {
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatal(err)
	}
	var m catalog.Manifest
	var v catalog.Variant
	for _, mm := range ms {
		for _, vv := range mm.Variants {
			if mm.ModelID == "qwen3.6-35b-a3b" && vv.VariantID == "mtp-q2-gguf" {
				m, v = mm, vv
			}
		}
	}
	if v.GGUF == nil || v.GGUF.NextNLayers == 0 || v.GGUF.DraftMaxTokens != 0 {
		t.Skipf("fixture changed: %+v", v.GGUF)
	}
	v.MTPDraftTokens = 2
	host := func(vramMB int) *agentInferenceProvider {
		return &agentInferenceProvider{logger: testLogger(), profiler: hardware.NewProfiler(t.TempDir(),
			hardware.WithOSArch(func() (string, string) { return "linux", "x86_64" }),
			hardware.WithRAM(func(context.Context) (int, int, error) { return 64, 60, nil }),
			// A discrete card on every OS: on an arm64 Mac the default
			// probe would mark the host unified and size it from RAM.
			hardware.WithUMA(func(context.Context, *hardware.Profile) {}),
			hardware.WithGPU(func(context.Context) ([]hardware.GPU, hardware.Accelerators, error) {
				return []hardware.GPU{{Vendor: "nvidia", Model: "test", VRAMTotalMB: vramMB, VRAMFreeMB: vramMB}}, hardware.Accelerators{CUDA: true}, nil
			}))}
	}
	ctx := context.Background()
	if got := host(48000).ollamaDraftToWrite(ctx, m, v); got != 2 {
		t.Errorf("48 GB card: draft %d, want 2", got)
	}
	if got := host(12000).ollamaDraftToWrite(ctx, m, v); got != 0 {
		t.Errorf("12 GB card, where the 13.5 GB weights already spill: draft %d, want 0", got)
	}
	none := v
	none.MTPDraftTokens = 0
	if got := host(48000).ollamaDraftToWrite(ctx, m, none); got != 0 {
		t.Errorf("no catalog draft: wrote %d", got)
	}
}

// Where one slot with the draft fits and two do not, the serve tuning gives
// up the second slot and keeps the draft (owner decision 2026-09-17,
// waired-ai/waired#1433): ollamaSlotsFit prices two slots with the draft
// the host would run.
func TestComputeOllamaTuning_TheDraftComesBeforeASecondSlot(t *testing.T) {
	ms, err := catalog.BundledManifests()
	if err != nil {
		t.Fatal(err)
	}
	var m catalog.Manifest
	var v catalog.Variant
	for _, mm := range ms {
		for _, vv := range mm.Variants {
			if mm.ModelID == "qwen3.6-35b-a3b" && vv.VariantID == "mtp-q2-gguf" {
				m, v = mm, vv
			}
		}
	}
	if v.GGUF == nil || v.GGUF.DraftMaxTokens != 0 {
		t.Skipf("fixture changed: %+v", v.GGUF)
	}
	mac24 := hardware.Profile{OS: "darwin", Arch: "arm64", RAMTotalGB: 24, UnifiedMemory: true, UsableVRAMMB: 18432,
		GPUs: []hardware.GPU{{Vendor: "apple", Model: "Apple M4"}}}
	without := computeOllamaTuning(m, v, mac24, "q4_0", ollamaObservedServe{})
	if without.NumParallel != 2 {
		t.Skipf("fixture no longer grants two slots without a draft here: %d", without.NumParallel)
	}
	v.MTPDraftTokens = 2
	with := computeOllamaTuning(m, v, mac24, "q4_0", ollamaObservedServe{})
	if d := hostfit.OllamaDraftTokens(v, mac24.HostFit(), with.KVCacheType, with.ContextLength); d != 2 || with.NumParallel != 1 {
		t.Errorf("draft %d with %d slots, want the draft (2) with one slot", d, with.NumParallel)
	}
	if with.ContextLength != without.ContextLength {
		t.Errorf("window %d with the draft, %d without: the window comes before the draft", with.ContextLength, without.ContextLength)
	}
}

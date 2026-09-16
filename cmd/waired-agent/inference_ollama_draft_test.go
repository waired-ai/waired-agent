package main

import (
	"context"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/download"
	"github.com/waired-ai/waired-agent/internal/platform/proclist"
	"github.com/waired-ai/waired-agent/proto/catalog"
)

// The draft the runner runs is recorded off its command line, and a tag
// the product writes a draft onto but whose runner runs none is pulled
// again once so the write happens (waired-ai/waired#1433).
func TestApplyOllamaTuningVerification_RecordsAndRepairsTheDraft(t *testing.T) {
	m, base, hw, tn := verifyFixture()
	weight := int64(10e9)
	size := weight + int64(0.5*65536*float64(verifyCtx))

	stamped := base
	gg := catalog.GGUFLayout{BlockCount: 41, NextNLayers: 1}
	stamped.GGUF = &gg
	stamped.MTPDraftTokens = 2
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
		wantRepair bool
	}{
		{"stamped-and-running", stamped, runner("--spec-type", "draft-mtp", "--spec-draft-n-max", "2"), "draft-mtp", 2, false},
		{"stamped-but-not-running", stamped, runner(), "", 0, true},
		{"publisher-draft-not-running", publisher, runner(), "", 0, false},
		{"no-draft-wanted", base, runner(), "", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := &fakeOllamaAPI{psName: verifyTag, psSize: size, psVRAM: size, psCtx: verifyCtx, tagSize: weight}
			srv := api.server(t)
			defer srv.Close()
			var repaired []string
			deps := ollamaVerifyDeps{ListProcs: c.procs, RestampDraft: func(tag string, v catalog.Variant) {
				repaired = append(repaired, tag+"/"+strconv.Itoa(v.MTPDraftTokens))
			}}
			sw := &fakeModelEnvSwitcher{}
			applyOllamaTuningVerification(context.Background(), sw, tn, m, c.v, hw,
				verifyTag, srv.URL, srv.Client(), deps, testLogger())
			got := sw.lastTuning(t)
			if got.SpeculativeMethod != c.wantMethod || got.SpeculativeTokens != c.wantTokens {
				t.Errorf("recorded draft %q/%d, want %q/%d", got.SpeculativeMethod, got.SpeculativeTokens, c.wantMethod, c.wantTokens)
			}
			if want := c.wantRepair; (len(repaired) == 1) != want || len(repaired) > 1 {
				t.Errorf("repairs = %v, want one: %v", repaired, want)
			}
			if c.wantRepair && len(repaired) == 1 && repaired[0] != verifyTag+"/2" {
				t.Errorf("repaired %q, want the serving tag with the variant's draft", repaired[0])
			}
		})
	}
}

// The repair pulls a tag at most once per process: the verification runs
// on every engine start and model switch, and a runner that still shows
// no draft after one re-pull would not change on a second.
func TestRestampDraft_PullsATagOnce(t *testing.T) {
	r := &draftPullRecorder{}
	p := &agentInferenceProvider{logger: testLogger()}
	p.puller = download.NewPuller("ollama-fake", r)
	v := catalog.Variant{VariantID: "mtp-q2-gguf", Renderer: "qwen3.5", Parser: "qwen3.5", MTPDraftTokens: 2}
	for range 3 {
		p.restampDraft("hf.co/ns/m:q2", v)
	}
	p.restampDraft("hf.co/ns/m:q3", v)
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

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// scaledManifest is the shape the shipped Qwen builds have after
// waired-ai/waired#1456: trained to 262,144, documented as reaching
// 1,048,576 through YaRN at factor 4.
func scaledManifest() catalog.Manifest {
	m := tuningTestManifest()
	m.RopeScaling = &catalog.RopeScaling{
		Type: catalog.RopeScalingYaRN, Factor: 4, OriginalContextLength: 262144,
		PublisherMaxContextLength: 1010000,
	}
	return m
}

// referenceClassHost is a unified-memory machine with room for the long
// window: weights plus a 1M KV cache, well inside the budget. The measured
// figures it stands in for are the reference host's — 29,560 MB resident at
// 1,048,576 for the default 35B-A3B build, against 23,638 MB at 200,704.
func referenceClassHost() hardware.Profile {
	return hardware.Profile{RAMTotalGB: 128, UnifiedMemory: true,
		GPUs: []hardware.GPU{{Vendor: "amd", VRAMTotalMB: 98304}}}
}

func envOf(t *testing.T, tune ollamaTuning) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, kv := range tune.Env() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("env entry %q is not KEY=VALUE", kv)
		}
		out[k] = v
	}
	return out
}

// Nobody asked: the host serves the window it always served, and the rope
// arguments are absent. This is the test that fails if the long window ever
// becomes something a computer takes on its own — static scaling is applied
// to every prompt the engine sees, so that would be a different model for
// an owner who never chose it.
func TestOllamaTuning_LongWindowNeedsAsking(t *testing.T) {
	m := scaledManifest()
	tune := computeOllamaTuningOpts(m, m.Variants[0], referenceClassHost(),
		ollamaTuningOpts{KVCacheType: "q4_0"})

	if tune.ContextLength != hostfit.ServingWindow200k {
		t.Errorf("ContextLength = %d, want the coding window %d", tune.ContextLength, hostfit.ServingWindow200k)
	}
	env := envOf(t, tune)
	for _, k := range []string{"LLAMA_ARG_ROPE_SCALING_TYPE", "LLAMA_ARG_ROPE_SCALE", "LLAMA_ARG_YARN_ORIG_CTX"} {
		if v, ok := env[k]; ok {
			t.Errorf("%s = %q on a host nobody asked for the long window", k, v)
		}
	}
	if env["OLLAMA_CONTEXT_LENGTH"] != "200704" {
		t.Errorf("OLLAMA_CONTEXT_LENGTH = %q, want 200704", env["OLLAMA_CONTEXT_LENGTH"])
	}
}

// Asked for, and the model documents the way there: the window opens and the
// engine is told how to get there.
func TestOllamaTuning_LongWindowWhenChosen(t *testing.T) {
	m := scaledManifest()
	tune := computeOllamaTuningOpts(m, m.Variants[0], referenceClassHost(),
		ollamaTuningOpts{KVCacheType: "q4_0", ChosenWindow: hostfit.ServingWindow1M})

	if tune.ContextLength != hostfit.ServingWindow1M {
		t.Fatalf("ContextLength = %d, want %d", tune.ContextLength, hostfit.ServingWindow1M)
	}
	env := envOf(t, tune)
	if env["OLLAMA_CONTEXT_LENGTH"] != "1048576" {
		t.Errorf("OLLAMA_CONTEXT_LENGTH = %q, want 1048576", env["OLLAMA_CONTEXT_LENGTH"])
	}
	if env["LLAMA_ARG_ROPE_SCALING_TYPE"] != "yarn" {
		t.Errorf("LLAMA_ARG_ROPE_SCALING_TYPE = %q", env["LLAMA_ARG_ROPE_SCALING_TYPE"])
	}
	if env["LLAMA_ARG_ROPE_SCALE"] != "4" {
		t.Errorf("LLAMA_ARG_ROPE_SCALE = %q, want 4", env["LLAMA_ARG_ROPE_SCALE"])
	}
	// The original length, not the model's context_length: after the stored
	// ceiling is raised the file reports the extended window as if it were
	// native, so the engine cannot recover this and must be told.
	if env["LLAMA_ARG_YARN_ORIG_CTX"] != "262144" {
		t.Errorf("LLAMA_ARG_YARN_ORIG_CTX = %q, want 262144", env["LLAMA_ARG_YARN_ORIG_CTX"])
	}
	// The engine derives attn_factor from the factor and cancels its own
	// kernel term; passing one applies it twice.
	if v, ok := env["LLAMA_ARG_YARN_ATTN_FACTOR"]; ok {
		t.Errorf("LLAMA_ARG_YARN_ATTN_FACTOR = %q; the engine derives it", v)
	}
}

// Asked for by someone whose model documents no way there: the request is
// not an error and not a promise. The host serves the coding window, and
// says nothing about scaling it cannot do.
func TestOllamaTuning_LongWindowAskedOfAModelThatCannotReachIt(t *testing.T) {
	m := tuningTestManifest() // no RopeScaling
	tune := computeOllamaTuningOpts(m, m.Variants[0], referenceClassHost(),
		ollamaTuningOpts{KVCacheType: "q4_0", ChosenWindow: hostfit.ServingWindow1M})

	if tune.ContextLength != hostfit.ServingWindow200k {
		t.Errorf("ContextLength = %d, want the coding window", tune.ContextLength)
	}
	if env := envOf(t, tune); env["LLAMA_ARG_ROPE_SCALING_TYPE"] != "" {
		t.Error("rope scaling was passed for a model that documents none")
	}
}

// Asked for on a machine too small to hold it: the planner falls back to the
// rung it can serve, and the scaling goes with it. A host that cannot hold
// the long window must not be told to extrapolate into memory it does not
// have.
func TestOllamaTuning_LongWindowOnASmallHost(t *testing.T) {
	m := scaledManifest()
	tune := computeOllamaTuningOpts(m, m.Variants[0], discrete24GB(),
		ollamaTuningOpts{KVCacheType: "q4_0", ChosenWindow: hostfit.ServingWindow1M})

	if tune.ContextLength == hostfit.ServingWindow1M {
		t.Fatal("a 24 GB card was given the long window")
	}
	if env := envOf(t, tune); env["LLAMA_ARG_ROPE_SCALING_TYPE"] != "" {
		t.Error("rope scaling was passed for a window this host is not serving")
	}
}

// The verify path lowers a ceiling; a choice raises one. They must not
// cancel each other out: a host stepped down after the long window failed
// to apply has to stay down, even though the choice is still on record.
func TestOllamaTuning_CeilingBeatsTheChoice(t *testing.T) {
	m := scaledManifest()
	tune := computeOllamaTuningOpts(m, m.Variants[0], referenceClassHost(),
		ollamaTuningOpts{KVCacheType: "q4_0", ChosenWindow: hostfit.ServingWindow1M,
			CeilingCtx: hostfit.ServingWindow200k})

	if tune.ContextLength != hostfit.ServingWindow200k {
		t.Errorf("ContextLength = %d, want the ceiling to win at %d", tune.ContextLength, hostfit.ServingWindow200k)
	}
	if env := envOf(t, tune); env["LLAMA_ARG_ROPE_SCALE"] != "" {
		t.Error("the scaling survived a ceiling that took the long window away")
	}
}

// Env is one list and the rope entries are additions to it, not a
// replacement: the KV type, the slot count and flash attention still go out.
func TestOllamaTuning_LongWindowKeepsTheRestOfTheEnv(t *testing.T) {
	m := scaledManifest()
	tune := computeOllamaTuningOpts(m, m.Variants[0], referenceClassHost(),
		ollamaTuningOpts{KVCacheType: "q4_0", ChosenWindow: hostfit.ServingWindow1M})
	env := envOf(t, tune)
	for _, k := range []string{"OLLAMA_KV_CACHE_TYPE", "OLLAMA_NUM_PARALLEL"} {
		if _, ok := env[k]; !ok {
			t.Errorf("%s is missing from the long-window env", k)
		}
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	t.Logf("long-window env: %v", keys)
}

// The declaration has to be able to say 1M, or no [1m] row can ever be
// answered — and it must still refuse to say it for a model that documents
// no way there, which is what the clamp did before waired-ai/waired#1456
// and still does.
//
// Driven through DeclaredContextWindow itself. An earlier version of this
// test recomputed the clamp beside the production one, which would have
// passed with the production path broken.
func TestDeclaredContextWindow_AgainstReach(t *testing.T) {
	manifests := []catalog.Manifest{
		{ModelID: "plain", ContextLength: 262144},
		{ModelID: "scaled", ContextLength: 262144, RopeScaling: &catalog.RopeScaling{
			Type: catalog.RopeScalingYaRN, Factor: 4, OriginalContextLength: 262144}},
	}
	prov := func(t *testing.T, model string, applied int) *agentInferenceProvider {
		t.Helper()
		a := newTestAdapter(t)
		a.SetAppliedTuning(infruntime.ModelTuning{ModelID: model, ContextLength: applied, WindowFits: true})
		store := catalog.NewStore(filepath.Join(t.TempDir(), "state.json"))
		if err := store.Update(func(s *catalog.State) {
			s.Models = map[string]catalog.ModelState{model: {State: catalog.ModelStateReady, VariantID: "v"}}
			s.Active = &catalog.ActiveSelection{
				Runtime: catalog.RuntimeOllama, ModelID: model, VariantID: "v", DecidedBy: "user"}
		}); err != nil {
			t.Fatalf("seed store: %v", err)
		}
		return &agentInferenceProvider{manifests: manifests, ollama: a, store: store}
	}

	for _, tc := range []struct {
		name    string
		model   string
		applied int
		want    int
	}{
		{"coding window, no scaling", "plain", hostfit.ServingWindow200k, hostfit.ServingWindow200k},
		{"coding window, scaling documented", "scaled", hostfit.ServingWindow200k, hostfit.ServingWindow200k},
		{"long window, scaling documented", "scaled", hostfit.ServingWindow1M, hostfit.ServingWindow1M},
		// A tuning above what the model can reach is a misconfiguration, and
		// the clamp is what stops it reaching the mesh as a promise.
		{"long window, no scaling", "plain", hostfit.ServingWindow1M, hostfit.ServingWindow200k},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := prov(t, tc.model, tc.applied)
			if got := p.DeclaredContextWindow(); got != tc.want {
				t.Errorf("DeclaredContextWindow = %d, want %d", got, tc.want)
			}
		})
	}
}

// storeWithStoredWindow lays out a model store holding one tag whose GGUF
// claims ctx, the way ollama does.
func storeWithStoredWindow(t *testing.T, tag string, ctx uint32) string {
	t.Helper()
	dir := t.TempDir()
	const digest = "sha256:4444444444444444444444444444444444444444444444444444444444444444"
	name, tagPart, _ := strings.Cut(tag, ":")
	mdir := filepath.Join(dir, "manifests", "registry.ollama.ai", "library", name)
	if err := os.MkdirAll(mdir, 0o755); err != nil {
		t.Fatal(err)
	}
	mf := `{"layers":[{"mediaType":"application/vnd.ollama.image.model","digest":"` + digest + `"}]}`
	if err := os.WriteFile(filepath.Join(mdir, tagPart), []byte(mf), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(dir, "blobs", "sha256-4444444444444444444444444444444444444444444444444444444444444444")
	if err := os.MkdirAll(filepath.Dir(blob), 0o755); err != nil {
		t.Fatal(err)
	}
	var b []byte
	str := func(s string) {
		b = binary.LittleEndian.AppendUint64(b, uint64(len(s)))
		b = append(b, s...)
	}
	b = append(b, "GGUF"...)
	b = binary.LittleEndian.AppendUint32(b, 3)
	b = binary.LittleEndian.AppendUint64(b, 0)
	b = binary.LittleEndian.AppendUint64(b, 2)
	str("general.architecture")
	b = binary.LittleEndian.AppendUint32(b, 8)
	str("qwen35moe")
	str("qwen35moe.context_length")
	b = binary.LittleEndian.AppendUint32(b, 4)
	b = binary.LittleEndian.AppendUint32(b, ctx)
	if err := os.WriteFile(blob, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The choice only becomes a request when this computer can actually serve it.
// The case that matters is the third: ollama clamps num_ctx to the file's own
// value, so asking against a file that claims less would leave the runner on
// the short window while the product declared the long one — the hole
// waired-ai/waired-agent#1436 describes, which is closed as not planned.
func TestOllamaWindowRequestFor(t *testing.T) {
	const tag = "qwen3.6:35b-a3b-mtp-q4_K_M"
	scaled := scaledManifest()
	scaled.ModelID = "scaled"
	v := scaled.Variants[0]
	v.Source = catalog.VariantSource{Type: catalog.SourceOllama, Tag: tag}
	plain := tuningTestManifest()
	plain.ModelID = "plain"
	pv := plain.Variants[0]
	pv.Source = v.Source

	chose1M := func(model string) agentconfig.InferenceConfig {
		return agentconfig.InferenceConfig{PreferredModelID: model,
			PreferredContextWindow: hostfit.ServingWindow1M}
	}

	t.Run("nobody chose anything", func(t *testing.T) {
		dir := storeWithStoredWindow(t, tag, 1048576)
		got, warn := ollamaWindowRequestFor(agentconfig.InferenceConfig{}, scaled, v, dir)
		if got != 0 || warn != "" {
			t.Errorf("got (%d, %q), want (0, \"\")", got, warn)
		}
	})

	t.Run("chosen, model reaches it, file claims it", func(t *testing.T) {
		dir := storeWithStoredWindow(t, tag, 1048576)
		got, warn := ollamaWindowRequestFor(chose1M("scaled"), scaled, v, dir)
		if got != hostfit.ServingWindow1M {
			t.Errorf("got %d, want %d", got, hostfit.ServingWindow1M)
		}
		if warn != "" {
			t.Errorf("unexpected warning: %q", warn)
		}
	})

	t.Run("chosen, but the stored file still claims the short window", func(t *testing.T) {
		dir := storeWithStoredWindow(t, tag, 262144)
		got, warn := ollamaWindowRequestFor(chose1M("scaled"), scaled, v, dir)
		if got != 0 {
			t.Errorf("got %d; asking for a window the file cannot serve is what #1436 is about", got)
		}
		if !strings.Contains(warn, "262144") || !strings.Contains(warn, "200k") {
			t.Errorf("the warning does not say what happened: %q", warn)
		}
	})

	t.Run("chosen, but nothing readable to check", func(t *testing.T) {
		got, warn := ollamaWindowRequestFor(chose1M("scaled"), scaled, v, t.TempDir())
		if got != 0 {
			t.Errorf("got %d with no readable build", got)
		}
		if warn == "" {
			t.Error("an unverifiable choice was taken silently")
		}
	})

	t.Run("chosen for a model that documents no way there", func(t *testing.T) {
		dir := storeWithStoredWindow(t, tag, 1048576)
		got, warn := ollamaWindowRequestFor(chose1M("plain"), plain, pv, dir)
		if got != 0 {
			t.Errorf("got %d for a model with no rope scaling", got)
		}
		// A stale choice outliving a model switch is not worth a warning on
		// the serving path; the row that offered it is what refuses it.
		if warn != "" {
			t.Errorf("unexpected warning: %q", warn)
		}
	})

	t.Run("the choice belongs to another model", func(t *testing.T) {
		dir := storeWithStoredWindow(t, tag, 1048576)
		got, _ := ollamaWindowRequestFor(chose1M("some-other-model"), scaled, v, dir)
		if got != 0 {
			t.Errorf("got %d; the choice names a different model", got)
		}
	})
}

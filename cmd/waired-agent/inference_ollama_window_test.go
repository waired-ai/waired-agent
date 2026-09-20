package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
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

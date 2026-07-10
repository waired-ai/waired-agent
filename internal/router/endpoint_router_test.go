package router

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/runtime"
)

// stubAdapter is a runtime.Adapter that exists for registry presence
// only; the router never calls its lifecycle methods.
type stubAdapter struct{ name string }

func (s stubAdapter) Name() string                        { return s.name }
func (s stubAdapter) EnsureRunning(context.Context) error { return nil }
func (s stubAdapter) Health(context.Context) runtime.Health {
	return runtime.Health{State: runtime.StateReady}
}
func (s stubAdapter) Stop(context.Context) error { return nil }
func (s stubAdapter) BaseURL() string            { return "http://stub" }

func qwen() catalog.Manifest {
	return catalog.Manifest{
		ModelID:       "qwen3-8b-instruct",
		ModelAliases:  []string{"waired/default", "waired/coding"},
		ContextLength: 8192,
		Capabilities:  []string{"chat", "json_mode"},
		Runtime:       catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
		Variants: []catalog.Variant{{
			VariantID:      "q4-gguf",
			Format:         catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			MinRAMGB:       12,
			Source:         catalog.VariantSource{Type: "ollama", Tag: "qwen3:8b-q4_K_M"},
		}},
	}
}

func readyState() catalog.State {
	return catalog.State{
		Version: catalog.StateVersion,
		Models: map[string]catalog.ModelState{
			"qwen3-8b-instruct": {
				VariantID: "q4-gguf",
				OllamaTag: "qwen3:8b-q4_K_M",
				State:     catalog.ModelStateReady,
				PulledAt:  time.Now(),
			},
		},
		Endpoints: map[string]catalog.EndpointState{},
	}
}

func goodHardware() hardware.Profile {
	return hardware.Profile{
		OS: "linux", Arch: "x86_64",
		CPU:        hardware.CPUInfo{Cores: 16},
		RAMTotalGB: 64, RAMAvailableGB: 48,
		Engines: hardware.InstalledEngines{Ollama: hardware.EngineInfo{Installed: true, Version: "0.22.1"}},
	}
}

func registryWithOllama() *runtime.Registry {
	r := runtime.NewRegistry()
	r.Register(stubAdapter{name: "ollama"})
	return r
}

func TestSelector_HappyPath(t *testing.T) {
	s := NewSelector(Inputs{
		Manifests:  []catalog.Manifest{qwen()},
		LocalState: readyState(),
		Hardware:   goodHardware(),
		Runtimes:   registryWithOllama(),
	})
	out, err := s.Select(context.Background(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if out.ModelID != "qwen3-8b-instruct" {
		t.Errorf("ModelID = %q", out.ModelID)
	}
	if out.Runtime != "ollama" {
		t.Errorf("Runtime = %q, want ollama", out.Runtime)
	}
	if out.VariantID != "q4-gguf" {
		t.Errorf("VariantID = %q", out.VariantID)
	}
	if out.ExecutionMode != "local" {
		t.Errorf("ExecutionMode = %q", out.ExecutionMode)
	}
	if !strings.HasPrefix(out.EndpointID, "ep_local_ollama_") {
		t.Errorf("EndpointID = %q (expected ep_local_ollama_ prefix)", out.EndpointID)
	}
	if out.EngineModel != "qwen3:8b-q4_K_M" {
		t.Errorf("EngineModel = %q, want qwen3:8b-q4_K_M", out.EngineModel)
	}
	if len(out.Decision.Reason) == 0 {
		t.Errorf("Decision.Reason should explain why this variant was picked")
	}
}

func TestSelector_LookupByExactModelID(t *testing.T) {
	s := NewSelector(Inputs{
		Manifests:  []catalog.Manifest{qwen()},
		LocalState: readyState(),
		Hardware:   goodHardware(),
		Runtimes:   registryWithOllama(),
	})
	out, err := s.Select(context.Background(), Request{Model: "qwen3-8b-instruct"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if out.ModelID != "qwen3-8b-instruct" {
		t.Errorf("ModelID = %q", out.ModelID)
	}
}

func TestSelector_UnknownModel(t *testing.T) {
	s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}, LocalState: readyState(), Hardware: goodHardware(), Runtimes: registryWithOllama()})
	_, err := s.Select(context.Background(), Request{Model: "waired/unknown"})
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("err = %v, want ErrModelNotFound", err)
	}
}

func TestSelector_ContextTooLarge(t *testing.T) {
	s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}, LocalState: readyState(), Hardware: goodHardware(), Runtimes: registryWithOllama()})
	_, err := s.Select(context.Background(), Request{
		Model:        "waired/default",
		Requirements: Requirements{MaxContextTokens: 100_000},
	})
	if !errors.Is(err, ErrCapabilityNotMet) {
		t.Errorf("err = %v, want ErrCapabilityNotMet", err)
	}
}

func TestSelector_NeedJSONMode(t *testing.T) {
	// qwen has json_mode → ok
	s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}, LocalState: readyState(), Hardware: goodHardware(), Runtimes: registryWithOllama()})
	if _, err := s.Select(context.Background(), Request{Model: "waired/default", Requirements: Requirements{NeedJSONMode: true}}); err != nil {
		t.Errorf("json_mode supported but Select rejected: %v", err)
	}

	// strip json_mode capability and re-test
	noJSON := qwen()
	noJSON.Capabilities = []string{"chat"}
	s2 := NewSelector(Inputs{Manifests: []catalog.Manifest{noJSON}, LocalState: readyState(), Hardware: goodHardware(), Runtimes: registryWithOllama()})
	if _, err := s2.Select(context.Background(), Request{Model: "waired/default", Requirements: Requirements{NeedJSONMode: true}}); !errors.Is(err, ErrCapabilityNotMet) {
		t.Errorf("expected ErrCapabilityNotMet, got %v", err)
	}
}

func TestSelector_HardwareInsufficientRAM(t *testing.T) {
	hw := goodHardware()
	hw.RAMTotalGB = 4 // less than min_ram_gb=12
	s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}, LocalState: readyState(), Hardware: hw, Runtimes: registryWithOllama()})
	_, err := s.Select(context.Background(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrHardwareInsufficient) {
		t.Errorf("err = %v, want ErrHardwareInsufficient", err)
	}
}

func TestSelector_ModelNotReady(t *testing.T) {
	st := readyState()
	m := st.Models["qwen3-8b-instruct"]
	m.State = catalog.ModelStateDownloading
	st.Models["qwen3-8b-instruct"] = m
	s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}, LocalState: st, Hardware: goodHardware(), Runtimes: registryWithOllama()})
	_, err := s.Select(context.Background(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotReady) {
		t.Errorf("err = %v, want ErrModelNotReady", err)
	}
}

func TestSelector_RuntimeNotInstalled(t *testing.T) {
	emptyReg := runtime.NewRegistry() // no ollama
	s := NewSelector(Inputs{Manifests: []catalog.Manifest{qwen()}, LocalState: readyState(), Hardware: goodHardware(), Runtimes: emptyReg})
	_, err := s.Select(context.Background(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrRuntimeNotInstalled) {
		t.Errorf("err = %v, want ErrRuntimeNotInstalled", err)
	}
}

func TestEndpointID_Stable(t *testing.T) {
	got1 := computeEndpointID("local", "ollama", "qwen3-8b-instruct")
	got2 := computeEndpointID("local", "ollama", "qwen3-8b-instruct")
	if got1 != got2 {
		t.Errorf("EndpointID not stable: %q vs %q", got1, got2)
	}
	if !strings.HasPrefix(got1, "ep_local_ollama_") {
		t.Errorf("EndpointID = %q", got1)
	}
}

func TestEndpointID_SanitizesModelID(t *testing.T) {
	got := computeEndpointID("local", "ollama", "Org/Model:8b-Q4_K_M")
	for _, c := range got {
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_'
		if !ok {
			t.Errorf("EndpointID %q has unexpected char %q", got, c)
			break
		}
	}
}

// bigModel is a second manifest with no static aliases, standing in
// for the host's actually-selected bundled/active model (#632).
func bigModel() catalog.Manifest {
	return catalog.Manifest{
		ModelID:       "qwen3-35b-moe",
		ContextLength: 262144,
		Capabilities:  []string{"chat", "json_mode"},
		Runtime:       catalog.RuntimePolicy{Preferred: catalog.RuntimeOllama},
		Variants: []catalog.Variant{{
			VariantID:      "mtp-q4-gguf",
			Format:         catalog.FormatOllamaTag,
			RuntimeSupport: []string{catalog.RuntimeOllama},
			MinRAMGB:       32,
			Source:         catalog.VariantSource{Type: "ollama", Tag: "qwen3:35b-mtp-q4_K_M"},
		}},
	}
}

func bigReadyState() catalog.State {
	return catalog.State{
		Version: catalog.StateVersion,
		Models: map[string]catalog.ModelState{
			"qwen3-35b-moe": {
				VariantID: "mtp-q4-gguf",
				OllamaTag: "qwen3:35b-mtp-q4_K_M",
				State:     catalog.ModelStateReady,
				PulledAt:  time.Now(),
			},
		},
		Endpoints: map[string]catalog.EndpointState{},
	}
}

// #632: waired/default must resolve to the host's coding default
// (DefaultModelID), not to whichever manifest happens to carry a
// static alias entry.
func TestSelector_DynamicDefaultAlias(t *testing.T) {
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{qwen(), bigModel()},
		LocalState:     bigReadyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		DefaultModelID: "qwen3-35b-moe",
	})
	for _, alias := range []string{"waired/default", "waired/coding"} {
		out, err := s.Select(context.Background(), Request{Model: alias})
		if err != nil {
			t.Fatalf("Select(%q): %v", alias, err)
		}
		if out.ModelID != "qwen3-35b-moe" {
			t.Errorf("Select(%q) ModelID = %q, want qwen3-35b-moe", alias, out.ModelID)
		}
		found := false
		for _, r := range out.Decision.Reason {
			if strings.Contains(r, "coding default") {
				found = true
			}
		}
		if !found {
			t.Errorf("Select(%q) reasons lack a dynamic-resolution line: %v", alias, out.Decision.Reason)
		}
	}
}

// Without a DefaultModelID (old callers, tests), the static alias
// entry keeps working — pure fallback, no behavior change.
func TestSelector_DynamicDefaultAlias_StaticFallback(t *testing.T) {
	s := NewSelector(Inputs{
		Manifests:  []catalog.Manifest{qwen(), bigModel()},
		LocalState: readyState(),
		Hardware:   goodHardware(),
		Runtimes:   registryWithOllama(),
	})
	out, err := s.Select(context.Background(), Request{Model: "waired/default"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if out.ModelID != "qwen3-8b-instruct" {
		t.Errorf("ModelID = %q, want static alias owner qwen3-8b-instruct", out.ModelID)
	}
}

// A DefaultModelID pointing at a manifest that does not exist falls
// back to static lookup; with no static owner either, the request is
// ErrModelNotFound (never a guess).
func TestSelector_DynamicDefaultAlias_UnknownDefault(t *testing.T) {
	s := NewSelector(Inputs{
		Manifests:      []catalog.Manifest{bigModel()},
		LocalState:     bigReadyState(),
		Hardware:       goodHardware(),
		Runtimes:       registryWithOllama(),
		DefaultModelID: "ghost-model",
	})
	_, err := s.Select(context.Background(), Request{Model: "waired/default"})
	if !errors.Is(err, ErrModelNotFound) {
		t.Fatalf("err = %v, want ErrModelNotFound", err)
	}
}

package agentconfig

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()

	// PRODUCT CONTRACT, not a record of today's value: there is no
	// compiled-in bundled model. The id is the OUTPUT of hardware-aware
	// selection (setup.SelectBundledModel), and a constant chosen before
	// the host is known cannot be right — the last one named a 32k-window
	// model that the same binary's picker excludes on every host it can
	// run on. A non-empty default here would be pulled verbatim on the
	// paths that skip selection.
	if cfg.Inference.BundledModelID != "" {
		t.Errorf("BundledModelID default = %q, want empty (chosen from the host, not compiled in)",
			cfg.Inference.BundledModelID)
	}
	if !cfg.Inference.PullOnStartup {
		t.Errorf("PullOnStartup default = false, want true")
	}
	// 0 = hold the model in memory indefinitely. Product contract:
	// owner ruling on waired-agent#861, recorded in
	// docs/decisions/20260820/0130-model-residency-is-a-setting.md.
	// This assertion previously pinned 10m, which no consumer read.
	if cfg.Inference.IdleTimeout.Duration() != 0 {
		t.Errorf("IdleTimeout default = %v, want 0 (indefinite)", cfg.Inference.IdleTimeout.Duration())
	}
	if cfg.Inference.MaxCacheGB != 100 {
		t.Errorf("MaxCacheGB default = %d, want 100", cfg.Inference.MaxCacheGB)
	}
	if !cfg.Inference.AllowPull {
		t.Errorf("AllowPull default = false, want true")
	}
	if !cfg.Inference.AllowAnthropicAPI {
		t.Errorf("AllowAnthropicAPI default = false, want true")
	}
	if !cfg.Inference.AllowOpenAIAPI {
		t.Errorf("AllowOpenAIAPI default = false, want true")
	}
	if cfg.Inference.LocalGatewayPort != 9473 {
		t.Errorf("LocalGatewayPort default = %d, want 9473", cfg.Inference.LocalGatewayPort)
	}
	if cfg.Inference.ClaudeGatewayPort != 9472 {
		t.Errorf("ClaudeGatewayPort default = %d, want 9472", cfg.Inference.ClaudeGatewayPort)
	}
	if cfg.Inference.OllamaPort != OllamaPortAuto {
		t.Errorf("OllamaPort default = %d, want %d (auto)", cfg.Inference.OllamaPort, OllamaPortAuto)
	}
	if cfg.Inference.ResolvedOllamaPort() != DefaultOllamaBundledPort {
		t.Errorf("ResolvedOllamaPort default = %d, want %d", cfg.Inference.ResolvedOllamaPort(), DefaultOllamaBundledPort)
	}
	// INVERTED by waired-agent#1026: the default was a literal 8000, vLLM's
	// own, which a Docker publish range or a dev server takes on an
	// ordinary Linux box — and a busy port is fatal for this engine.
	if cfg.Inference.VLLMPort != VLLMPortAuto {
		t.Errorf("VLLMPort default = %d, want %d (auto)", cfg.Inference.VLLMPort, VLLMPortAuto)
	}
	if cfg.Inference.ResolvedVLLMPort() != DefaultVLLMBundledPort {
		t.Errorf("ResolvedVLLMPort default = %d, want %d", cfg.Inference.ResolvedVLLMPort(), DefaultVLLMBundledPort)
	}
	if cfg.Inference.VLLMGPUMemoryUtilization != 0.85 {
		t.Errorf("VLLMGPUMemoryUtilization default = %v, want 0.85", cfg.Inference.VLLMGPUMemoryUtilization)
	}
	if cfg.Inference.VLLMTensorParallel != 0 {
		t.Errorf("VLLMTensorParallel default = %d, want 0 (auto)", cfg.Inference.VLLMTensorParallel)
	}
	if cfg.Inference.VLLMDisableFP8KV {
		t.Errorf("VLLMDisableFP8KV default = true, want false (fp8 on for Ada+)")
	}
	if cfg.Inference.VLLMSpeculativeNgram {
		t.Errorf("VLLMSpeculativeNgram default = true, want false (opt-in)")
	}
	if cfg.Inference.PreferredEngine != "" {
		t.Errorf("PreferredEngine default = %q, want empty (auto)", cfg.Inference.PreferredEngine)
	}
	if cfg.Inference.PreferredModelID != "" {
		t.Errorf("PreferredModelID default = %q, want empty (auto)", cfg.Inference.PreferredModelID)
	}
	if !cfg.Inference.AllowAutoFallback {
		t.Errorf("AllowAutoFallback default = false, want true")
	}
	if !cfg.Inference.PreCacheUpdateCandidate {
		t.Errorf("PreCacheUpdateCandidate default = false, want true")
	}
	if !cfg.Inference.Enabled {
		t.Errorf("Enabled default = false, want true")
	}
	if !cfg.Inference.ShareWithMesh {
		t.Errorf("ShareWithMesh default = false, want true")
	}
}

func TestMergeJSON_FileNotFound(t *testing.T) {
	cfg := Defaults()
	missing := filepath.Join(t.TempDir(), "does-not-exist.json")
	if err := cfg.MergeJSON(missing); err != nil {
		t.Fatalf("MergeJSON for missing file should succeed (use defaults), got %v", err)
	}
	// defaults preserved. MaxCacheGB rather than BundledModelID: the
	// latter's default is the zero value now, so it could not tell a
	// preserved default from a wiped struct.
	if cfg.Inference.MaxCacheGB != 100 {
		t.Errorf("MergeJSON corrupted defaults: MaxCacheGB = %d", cfg.Inference.MaxCacheGB)
	}
}

func TestMergeJSON_Override(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	body := `{
		"inference": {
			"bundled_model_id": "custom-model",
			"pull_on_startup": false,
			"idle_timeout": "30m",
			"max_cache_gb": 50,
			"allow_pull": false,
			"allow_anthropic_api": false,
			"allow_openai_api": false,
			"local_gateway_port": 19473,
			"ollama_port": 21434
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatalf("MergeJSON: %v", err)
	}

	if cfg.Inference.BundledModelID != "custom-model" {
		t.Errorf("BundledModelID = %q", cfg.Inference.BundledModelID)
	}
	if cfg.Inference.PullOnStartup {
		t.Errorf("PullOnStartup should be false after JSON override")
	}
	// The body has carried allow_pull since this test was written and
	// nothing asserted it. #338 narrowed the field to "download weights"
	// alone, which leaves this merge as the only thing standing between a
	// boot and a multi-GB download.
	if cfg.Inference.AllowPull {
		t.Errorf("AllowPull should be false after JSON override")
	}
	if cfg.Inference.IdleTimeout.Duration() != 30*time.Minute {
		t.Errorf("IdleTimeout = %v", cfg.Inference.IdleTimeout.Duration())
	}
	if cfg.Inference.MaxCacheGB != 50 {
		t.Errorf("MaxCacheGB = %d", cfg.Inference.MaxCacheGB)
	}
	if cfg.Inference.LocalGatewayPort != 19473 {
		t.Errorf("LocalGatewayPort = %d", cfg.Inference.LocalGatewayPort)
	}
	if cfg.Inference.OllamaPort != 21434 {
		t.Errorf("OllamaPort = %d", cfg.Inference.OllamaPort)
	}
}

func TestMergeJSON_Step2Fields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	body := `{
		"inference": {
			"vllm_port": 9000,
			"vllm_gpu_memory_utilization": 0.92,
			"vllm_tensor_parallel": 2,
			"vllm_disable_fp8_kv": true,
			"vllm_speculative_ngram": true,
			"preferred_engine": "vllm",
			"preferred_model_id": "qwen3-14b-instruct",
			"interactive_floor_tokps": 42.5,
			"allow_auto_fallback": false,
			"pre_cache_update_candidate": false
		}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatalf("MergeJSON: %v", err)
	}
	if cfg.Inference.VLLMPort != 9000 {
		t.Errorf("VLLMPort = %d, want 9000", cfg.Inference.VLLMPort)
	}
	if cfg.Inference.VLLMGPUMemoryUtilization != 0.92 {
		t.Errorf("VLLMGPUMemoryUtilization = %v, want 0.92", cfg.Inference.VLLMGPUMemoryUtilization)
	}
	if cfg.Inference.VLLMTensorParallel != 2 {
		t.Errorf("VLLMTensorParallel = %d, want 2", cfg.Inference.VLLMTensorParallel)
	}
	if !cfg.Inference.VLLMDisableFP8KV {
		t.Errorf("VLLMDisableFP8KV = false, want true after JSON override")
	}
	if !cfg.Inference.VLLMSpeculativeNgram {
		t.Errorf("VLLMSpeculativeNgram = false, want true after JSON override")
	}
	if cfg.Inference.PreferredEngine != "vllm" {
		t.Errorf("PreferredEngine = %q, want vllm", cfg.Inference.PreferredEngine)
	}
	if cfg.Inference.PreferredModelID != "qwen3-14b-instruct" {
		t.Errorf("PreferredModelID = %q, want qwen3-14b-instruct", cfg.Inference.PreferredModelID)
	}
	if cfg.Inference.InteractiveFloorTokps != 42.5 {
		t.Errorf("InteractiveFloorTokps = %v, want 42.5", cfg.Inference.InteractiveFloorTokps)
	}
	if cfg.Inference.AllowAutoFallback {
		t.Errorf("AllowAutoFallback should be false after JSON override")
	}
	if cfg.Inference.PreCacheUpdateCandidate {
		t.Errorf("PreCacheUpdateCandidate should be false after JSON override")
	}
}

func TestMergeEnv_Step2Fields(t *testing.T) {
	cfg := Defaults()
	env := []string{
		"WAIRED_INFERENCE_VLLM_PORT=9000",
		"WAIRED_INFERENCE_VLLM_GPU_MEMORY_UTILIZATION=0.93",
		"WAIRED_INFERENCE_VLLM_TENSOR_PARALLEL=4",
		"WAIRED_INFERENCE_VLLM_DISABLE_FP8_KV=true",
		"WAIRED_INFERENCE_VLLM_SPECULATIVE_NGRAM=true",
		"WAIRED_INFERENCE_PREFERRED_ENGINE=ollama",
		"WAIRED_INFERENCE_PREFERRED_MODEL_ID=qwen3-7b-instruct",
		"WAIRED_INFERENCE_INTERACTIVE_FLOOR_TOKPS=18.5",
		"WAIRED_INFERENCE_ALLOW_AUTO_FALLBACK=false",
		"WAIRED_INFERENCE_PRE_CACHE_UPDATE_CANDIDATE=false",
	}
	if err := cfg.MergeEnv(env); err != nil {
		t.Fatalf("MergeEnv: %v", err)
	}
	if cfg.Inference.VLLMPort != 9000 {
		t.Errorf("VLLMPort = %d", cfg.Inference.VLLMPort)
	}
	if cfg.Inference.VLLMGPUMemoryUtilization != 0.93 {
		t.Errorf("VLLMGPUMemoryUtilization = %v", cfg.Inference.VLLMGPUMemoryUtilization)
	}
	if cfg.Inference.VLLMTensorParallel != 4 {
		t.Errorf("VLLMTensorParallel = %d, want 4", cfg.Inference.VLLMTensorParallel)
	}
	if !cfg.Inference.VLLMDisableFP8KV {
		t.Errorf("VLLMDisableFP8KV = false, want true after env override")
	}
	if !cfg.Inference.VLLMSpeculativeNgram {
		t.Errorf("VLLMSpeculativeNgram = false, want true after env override")
	}
	if cfg.Inference.PreferredEngine != "ollama" {
		t.Errorf("PreferredEngine = %q", cfg.Inference.PreferredEngine)
	}
	if cfg.Inference.PreferredModelID != "qwen3-7b-instruct" {
		t.Errorf("PreferredModelID = %q", cfg.Inference.PreferredModelID)
	}
	if cfg.Inference.InteractiveFloorTokps != 18.5 {
		t.Errorf("InteractiveFloorTokps = %v, want 18.5", cfg.Inference.InteractiveFloorTokps)
	}
	if cfg.Inference.AllowAutoFallback {
		t.Errorf("AllowAutoFallback should be false")
	}
	if cfg.Inference.PreCacheUpdateCandidate {
		t.Errorf("PreCacheUpdateCandidate should be false")
	}
}

func TestValidate_VLLMGPUMemoryUtilization(t *testing.T) {
	cases := []struct {
		name    string
		value   float64
		wantErr bool
	}{
		{"default 0.85", 0.85, false},
		{"max 1.0", 1.0, false},
		{"min positive 0.01", 0.01, false},
		{"zero rejected", 0.0, true},
		{"negative rejected", -0.1, true},
		{"above 1 rejected", 1.01, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Inference.VLLMGPUMemoryUtilization = tc.value
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%v) = nil, want error", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%v) = %v, want nil", tc.value, err)
			}
		})
	}
}

func TestValidate_VLLMTensorParallel(t *testing.T) {
	cases := []struct {
		name    string
		value   int
		wantErr bool
	}{
		{"default 0 (auto)", 0, false},
		{"force single GPU", 1, false},
		{"explicit 2", 2, false},
		{"negative rejected", -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Inference.VLLMTensorParallel = tc.value
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%d) = nil, want error", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%d) = %v, want nil", tc.value, err)
			}
		})
	}
}

func TestValidate_InteractiveFloorTokps(t *testing.T) {
	cases := []struct {
		name    string
		value   float64
		wantErr bool
	}{
		{"default 0 (use built-in)", 0, false},
		{"positive override", 20, false},
		{"negative rejected", -1, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Inference.InteractiveFloorTokps = tc.value
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%v) = nil, want error", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%v) = %v, want nil", tc.value, err)
			}
		})
	}
}

func TestValidate_PreferredEngine(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"empty (auto)", "", false},
		{"ollama", "ollama", false},
		{"vllm", "vllm", false},
		{"invalid", "tensorrt", true},
		{"case mismatch", "VLLM", true}, // strict lowercase
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Inference.PreferredEngine = tc.value
			err := cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%q) = nil, want error", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tc.value, err)
			}
		})
	}
}

func TestRegisterInferenceFlags_Step2Fields(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg.RegisterInferenceFlags(fs)
	if err := fs.Parse([]string{
		"--inference-vllm-port=9000",
		"--inference-vllm-gpu-memory-utilization=0.95",
		"--inference-vllm-tensor-parallel=2",
		"--inference-preferred-engine=vllm",
		"--inference-preferred-model-id=qwen3-32b-instruct",
		"--inference-interactive-floor-tokps=25",
		"--inference-allow-auto-fallback=false",
		"--inference-pre-cache-update-candidate=false",
	}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Inference.InteractiveFloorTokps != 25 {
		t.Errorf("InteractiveFloorTokps flag override = %v, want 25", cfg.Inference.InteractiveFloorTokps)
	}
	if cfg.Inference.VLLMPort != 9000 {
		t.Errorf("VLLMPort flag override = %d", cfg.Inference.VLLMPort)
	}
	if cfg.Inference.VLLMGPUMemoryUtilization != 0.95 {
		t.Errorf("VLLMGPUMemoryUtilization flag override = %v", cfg.Inference.VLLMGPUMemoryUtilization)
	}
	if cfg.Inference.VLLMTensorParallel != 2 {
		t.Errorf("VLLMTensorParallel flag override = %d, want 2", cfg.Inference.VLLMTensorParallel)
	}
	if cfg.Inference.PreferredEngine != "vllm" {
		t.Errorf("PreferredEngine flag override = %q", cfg.Inference.PreferredEngine)
	}
	if cfg.Inference.PreferredModelID != "qwen3-32b-instruct" {
		t.Errorf("PreferredModelID flag override = %q", cfg.Inference.PreferredModelID)
	}
	if cfg.Inference.AllowAutoFallback {
		t.Errorf("AllowAutoFallback flag override = true")
	}
	if cfg.Inference.PreCacheUpdateCandidate {
		t.Errorf("PreCacheUpdateCandidate flag override = true")
	}
}

func TestMergeJSON_PartialOverride(t *testing.T) {
	// Only one field overridden; others must stay at defaults.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(path, []byte(`{"inference":{"bundled_model_id":"only-this"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatalf("MergeJSON: %v", err)
	}
	if cfg.Inference.BundledModelID != "only-this" {
		t.Errorf("BundledModelID = %q", cfg.Inference.BundledModelID)
	}
	// Other fields must be untouched. Note IdleTimeout's default is now
	// the zero value, so it can no longer distinguish "untouched" from
	// "clobbered to zero" — MaxCacheGB below carries that duty.
	if cfg.Inference.IdleTimeout.Duration() != 0 {
		t.Errorf("IdleTimeout was clobbered: %v", cfg.Inference.IdleTimeout.Duration())
	}
	if cfg.Inference.MaxCacheGB != 100 {
		t.Errorf("MaxCacheGB was clobbered: %d", cfg.Inference.MaxCacheGB)
	}
}

func TestMergeJSON_BadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte(`{not valid`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := Defaults()
	if err := cfg.MergeJSON(path); err == nil {
		t.Errorf("expected error for malformed JSON, got nil")
	}
}

func TestMergeEnv(t *testing.T) {
	cfg := Defaults()
	env := []string{
		"WAIRED_INFERENCE_BUNDLED_MODEL_ID=env-model",
		"WAIRED_INFERENCE_PULL_ON_STARTUP=false",
		"WAIRED_INFERENCE_IDLE_TIMEOUT=15m",
		"WAIRED_INFERENCE_MAX_CACHE_GB=200",
		"WAIRED_INFERENCE_ALLOW_PULL=false",
		"WAIRED_INFERENCE_ALLOW_ANTHROPIC_API=false",
		"WAIRED_INFERENCE_ALLOW_OPENAI_API=false",
		"WAIRED_INFERENCE_LOCAL_GATEWAY_PORT=8473",
		"WAIRED_INFERENCE_OLLAMA_PORT=8434",
		"UNRELATED=ignored",
	}
	if err := cfg.MergeEnv(env); err != nil {
		t.Fatalf("MergeEnv: %v", err)
	}
	if cfg.Inference.BundledModelID != "env-model" {
		t.Errorf("BundledModelID = %q", cfg.Inference.BundledModelID)
	}
	if cfg.Inference.PullOnStartup {
		t.Errorf("PullOnStartup should be false")
	}
	// Same gap as TestMergeJSON_Override's: the env var was in the fixture
	// with no assertion behind it, so setInferenceField's ALLOW_PULL case
	// was uncovered (#338).
	if cfg.Inference.AllowPull {
		t.Errorf("AllowPull should be false after env override")
	}
	if cfg.Inference.IdleTimeout.Duration() != 15*time.Minute {
		t.Errorf("IdleTimeout = %v", cfg.Inference.IdleTimeout.Duration())
	}
	if cfg.Inference.MaxCacheGB != 200 {
		t.Errorf("MaxCacheGB = %d", cfg.Inference.MaxCacheGB)
	}
	if cfg.Inference.LocalGatewayPort != 8473 {
		t.Errorf("LocalGatewayPort = %d", cfg.Inference.LocalGatewayPort)
	}
	if cfg.Inference.OllamaPort != 8434 {
		t.Errorf("OllamaPort = %d", cfg.Inference.OllamaPort)
	}
}

func TestMergeEnv_BadDuration(t *testing.T) {
	cfg := Defaults()
	if err := cfg.MergeEnv([]string{"WAIRED_INFERENCE_IDLE_TIMEOUT=not-a-duration"}); err == nil {
		t.Errorf("expected error for malformed duration, got nil")
	}
}

func TestMergeEnv_BadInt(t *testing.T) {
	cfg := Defaults()
	if err := cfg.MergeEnv([]string{"WAIRED_INFERENCE_MAX_CACHE_GB=lots"}); err == nil {
		t.Errorf("expected error for malformed int, got nil")
	}
}

func TestRegisterInferenceFlags_Override(t *testing.T) {
	cfg := Defaults()
	cfg.Inference.IdleTimeout = NewDuration(5 * time.Minute) // simulate prior layer
	cfg.Inference.LocalGatewayPort = 12345

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg.RegisterInferenceFlags(fs)

	// Only override one of the two pre-set fields via flags.
	if err := fs.Parse([]string{"--inference-local-gateway-port=9999"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if cfg.Inference.LocalGatewayPort != 9999 {
		t.Errorf("LocalGatewayPort flag override = %d, want 9999", cfg.Inference.LocalGatewayPort)
	}
	// IdleTimeout was not set via flag, so the prior layer value must persist.
	if cfg.Inference.IdleTimeout.Duration() != 5*time.Minute {
		t.Errorf("IdleTimeout was clobbered by flag default: %v", cfg.Inference.IdleTimeout.Duration())
	}
}

func TestRegisterInferenceFlags_BoolToggle(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg.RegisterInferenceFlags(fs)
	if err := fs.Parse([]string{"--inference-allow-anthropic-api=false", "--inference-pull-on-startup=false"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Inference.AllowAnthropicAPI {
		t.Errorf("AllowAnthropicAPI should be false after flag override")
	}
	if cfg.Inference.PullOnStartup {
		t.Errorf("PullOnStartup should be false after flag override")
	}
}

func TestPrecedence_FlagOverridesEnvOverridesJSON(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(jsonPath, []byte(`{"inference":{"local_gateway_port":1111,"ollama_port":2222,"bundled_model_id":"json-model"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg := Defaults()
	if err := cfg.MergeJSON(jsonPath); err != nil {
		t.Fatalf("MergeJSON: %v", err)
	}
	if err := cfg.MergeEnv([]string{
		"WAIRED_INFERENCE_OLLAMA_PORT=3333",
		"WAIRED_INFERENCE_BUNDLED_MODEL_ID=env-model",
	}); err != nil {
		t.Fatalf("MergeEnv: %v", err)
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg.RegisterInferenceFlags(fs)
	if err := fs.Parse([]string{"--inference-bundled-model-id=flag-model"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// JSON-only field
	if cfg.Inference.LocalGatewayPort != 1111 {
		t.Errorf("LocalGatewayPort (json only) = %d, want 1111", cfg.Inference.LocalGatewayPort)
	}
	// JSON < env
	if cfg.Inference.OllamaPort != 3333 {
		t.Errorf("OllamaPort (env over json) = %d, want 3333", cfg.Inference.OllamaPort)
	}
	// JSON < env < flag
	if cfg.Inference.BundledModelID != "flag-model" {
		t.Errorf("BundledModelID (flag over env) = %q, want flag-model", cfg.Inference.BundledModelID)
	}
}

func TestDuration_JSONRoundTrip(t *testing.T) {
	d := NewDuration(7 * time.Minute)
	enc, err := d.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if string(enc) != `"7m0s"` {
		t.Errorf("MarshalJSON = %s, want \"7m0s\"", enc)
	}

	var d2 Duration
	if err := d2.UnmarshalJSON([]byte(`"3h"`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if d2.Duration() != 3*time.Hour {
		t.Errorf("UnmarshalJSON = %v, want 3h", d2.Duration())
	}

	if err := d2.UnmarshalJSON([]byte(`"not-a-duration"`)); err == nil {
		t.Errorf("expected error for malformed duration string")
	}
}

func TestDefaultJSONPath(t *testing.T) {
	// DefaultJSONPath delegates to platform/paths.StateDir; the only
	// thing we promise is that it appends "agent.json" to whatever the
	// platform resolver returns. Use $WAIRED_STATE_DIR (the override
	// path that bypasses every OS-specific branch) to make the test
	// portable across Linux / macOS / Windows.
	dir := t.TempDir()
	t.Setenv("WAIRED_STATE_DIR", dir)
	got := DefaultJSONPath()
	want := filepath.Join(dir, "agent.json")
	if got != want {
		t.Errorf("DefaultJSONPath = %q, want %q", got, want)
	}
}

func TestJSONPathFor(t *testing.T) {
	dir := t.TempDir()
	got := JSONPathFor(dir)
	want := filepath.Join(dir, "agent.json")
	if got != want {
		t.Errorf("JSONPathFor = %q, want %q", got, want)
	}
}

func TestConfig_Save_MergeJSONRoundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")

	original := Defaults()
	original.Inference.Enabled = false
	original.Inference.ShareWithMesh = false
	original.Inference.BundledModelID = "qwen2.5-coder-3b-instruct"

	if err := original.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	roundtrip := Defaults()
	if err := roundtrip.MergeJSON(path); err != nil {
		t.Fatalf("MergeJSON: %v", err)
	}

	if roundtrip.Inference.Enabled != false {
		t.Errorf("Enabled = %v, want false", roundtrip.Inference.Enabled)
	}
	if roundtrip.Inference.ShareWithMesh != false {
		t.Errorf("ShareWithMesh = %v, want false", roundtrip.Inference.ShareWithMesh)
	}
	if roundtrip.Inference.BundledModelID != "qwen2.5-coder-3b-instruct" {
		t.Errorf("BundledModelID = %q, want qwen2.5-coder-3b-instruct",
			roundtrip.Inference.BundledModelID)
	}
}

// Phase 6 — Enabled + ShareWithMesh coverage across all four merge layers.

func TestPhase6Fields_JSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	body := `{"inference":{"enabled":false,"share_with_mesh":false}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatalf("MergeJSON: %v", err)
	}
	if cfg.Inference.Enabled {
		t.Errorf("Enabled should be false after JSON override")
	}
	if cfg.Inference.ShareWithMesh {
		t.Errorf("ShareWithMesh should be false after JSON override")
	}
}

func TestPhase6Fields_JSON_PartialPreservesDefaults(t *testing.T) {
	// JSON omits both fields; defaults (=true) must survive the merge.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	body := `{"inference":{"bundled_model_id":"foo"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatalf("MergeJSON: %v", err)
	}
	if !cfg.Inference.Enabled {
		t.Errorf("Enabled default was clobbered by partial JSON, want true")
	}
	if !cfg.Inference.ShareWithMesh {
		t.Errorf("ShareWithMesh default was clobbered by partial JSON, want true")
	}
}

func TestPhase6Fields_Env(t *testing.T) {
	cfg := Defaults()
	env := []string{
		"WAIRED_INFERENCE_ENABLED=false",
		"WAIRED_INFERENCE_SHARE_WITH_MESH=false",
	}
	if err := cfg.MergeEnv(env); err != nil {
		t.Fatalf("MergeEnv: %v", err)
	}
	if cfg.Inference.Enabled {
		t.Errorf("Enabled should be false via env")
	}
	if cfg.Inference.ShareWithMesh {
		t.Errorf("ShareWithMesh should be false via env")
	}
}

func TestPhase6Fields_Env_BadBool(t *testing.T) {
	cfg := Defaults()
	if err := cfg.MergeEnv([]string{"WAIRED_INFERENCE_ENABLED=maybe"}); err == nil {
		t.Errorf("expected error for malformed bool, got nil")
	}
	if err := cfg.MergeEnv([]string{"WAIRED_INFERENCE_SHARE_WITH_MESH=maybe"}); err == nil {
		t.Errorf("expected error for malformed bool, got nil")
	}
}

func TestPhase6Fields_Flags(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg.RegisterInferenceFlags(fs)
	if err := fs.Parse([]string{
		"--inference-enabled=false",
		"--inference-share-with-mesh=false",
	}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Inference.Enabled {
		t.Errorf("Enabled flag override should land false")
	}
	if cfg.Inference.ShareWithMesh {
		t.Errorf("ShareWithMesh flag override should land false")
	}
}

func TestResolvedOllamaPort(t *testing.T) {
	cases := []struct {
		name string
		port int
		want int
	}{
		{"auto", OllamaPortAuto, DefaultOllamaBundledPort},
		// Every pre-existing agent.json serialized the old shared default
		// (11434) explicitly, so a literal 11434 is indistinguishable from
		// "never chose a port" — it flips to the waired-owned port.
		// "The engine on 11434" is no longer expressible, which is what
		// keeps a pre-#489 file off a user's own ollama.
		{"legacy 11434 flips", 11434, DefaultOllamaBundledPort},
		{"explicit custom kept", 8434, 8434},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := InferenceConfig{OllamaPort: tc.port}
			if got := c.ResolvedOllamaPort(); got != tc.want {
				t.Errorf("ResolvedOllamaPort(port=%d) = %d, want %d", tc.port, got, tc.want)
			}
		})
	}
}

// A pre-#489 agent.json carrying ollama_source: "reuse" must still LOAD —
// the daemon may never fail to boot on a key that no longer exists — and
// must resolve to the waired-managed engine on the waired-owned port.
// The key itself disappears from the file on the next Save.
func TestMergeJSON_RetiredOllamaSourceIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	body := `{"inference":{"ollama_source":"reuse","ollama_port":11434}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatalf("MergeJSON with a retired ollama_source must not fail: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate after loading a retired ollama_source: %v", err)
	}
	if got := cfg.Inference.ResolvedOllamaPort(); got != DefaultOllamaBundledPort {
		t.Errorf("ResolvedOllamaPort = %d, want %d (the waired-owned port, not the user's 11434)",
			got, DefaultOllamaBundledPort)
	}

	out := filepath.Join(dir, "written.json")
	if err := cfg.Save(out); err != nil {
		t.Fatalf("Save: %v", err)
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "ollama_source") {
		t.Errorf("Save re-emitted the retired key:\n%s", written)
	}
}

// Same contract for the other key #488 retired: a hand-edited
// agent.json carrying inference.external_endpoints must LOAD, must
// validate, and must lose the key on the next Save. Nothing in the
// agent acts on it any more — the entries are simply not there.
func TestMergeJSON_RetiredExternalEndpointsIsIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	body := `{"inference":{"external_endpoints":[` +
		`{"id":"lan","url":"http://192.168.1.10:8000/v1","auth_env_var":"LAN_KEY"}]}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatalf("MergeJSON with retired external_endpoints must not fail: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate after loading retired external_endpoints: %v", err)
	}

	out := filepath.Join(dir, "written.json")
	if err := cfg.Save(out); err != nil {
		t.Fatalf("Save: %v", err)
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "external_endpoints") {
		t.Errorf("Save re-emitted the retired key:\n%s", written)
	}
}

func TestResolvedVLLMPort(t *testing.T) {
	cases := []struct {
		name string
		port int
		want int
	}{
		{"auto", VLLMPortAuto, DefaultVLLMBundledPort},
		// Defaults() is serialized when agent.json is written, so every
		// host set up before waired-agent#1026 carries a literal 8000 that
		// cannot be told apart from "never chose a port" — the same
		// situation the 11434 flip above was written for. "The engine on
		// 8000" stops being expressible, which is the point: 8000 is the
		// port a Docker publish range or a dev server takes.
		{"legacy 8000 flips", 8000, DefaultVLLMBundledPort},
		{"explicit custom kept", 9485, 9485},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := InferenceConfig{VLLMPort: tc.port}
			if got := c.ResolvedVLLMPort(); got != tc.want {
				t.Errorf("ResolvedVLLMPort(port=%d) = %d, want %d", tc.port, got, tc.want)
			}
		})
	}
}

// The two engines must not be given the same port. They can be installed on
// the same host (waired-agent#339 adopts one after boot), and a collision
// would be a self-inflicted instance of the defect #1026 is about.
func TestBundledEnginePortsDoNotCollide(t *testing.T) {
	if DefaultOllamaBundledPort == DefaultVLLMBundledPort {
		t.Fatalf("both engines resolve to %d", DefaultOllamaBundledPort)
	}
	c := Defaults().Inference
	for name, port := range map[string]int{
		"local gateway":  c.LocalGatewayPort,
		"claude gateway": c.ClaudeGatewayPort,
	} {
		if port == DefaultVLLMBundledPort {
			t.Errorf("the vLLM engine port %d is also the %s port", port, name)
		}
	}
}

func TestValidate_VLLMPortRange(t *testing.T) {
	for _, bad := range []int{-1, 65536} {
		cfg := Defaults()
		cfg.Inference.VLLMPort = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate(VLLMPort=%d) = nil, want error", bad)
		}
	}
	for _, ok := range []int{VLLMPortAuto, 1, 9479, 8000, 65535} {
		cfg := Defaults()
		cfg.Inference.VLLMPort = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(VLLMPort=%d) = %v, want nil", ok, err)
		}
	}
}

func TestValidate_OllamaPortRange(t *testing.T) {
	for _, bad := range []int{-1, 65536} {
		cfg := Defaults()
		cfg.Inference.OllamaPort = bad
		if err := cfg.Validate(); err == nil {
			t.Errorf("Validate(OllamaPort=%d) = nil, want error", bad)
		}
	}
	for _, ok := range []int{OllamaPortAuto, 1, 9475, 11434, 65535} {
		cfg := Defaults()
		cfg.Inference.OllamaPort = ok
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate(OllamaPort=%d) = %v, want nil", ok, err)
		}
	}
}

package agentconfig

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaults(t *testing.T) {
	cfg := Defaults()

	if cfg.Inference.BundledModelID != "qwen2.5-coder-7b-instruct" {
		t.Errorf("BundledModelID default = %q, want qwen2.5-coder-7b-instruct", cfg.Inference.BundledModelID)
	}
	if !cfg.Inference.PullOnStartup {
		t.Errorf("PullOnStartup default = false, want true")
	}
	if cfg.Inference.IdleTimeout.Duration() != 10*time.Minute {
		t.Errorf("IdleTimeout default = %v, want 10m", cfg.Inference.IdleTimeout.Duration())
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
	if cfg.Inference.VLLMPort != 8000 {
		t.Errorf("VLLMPort default = %d, want 8000", cfg.Inference.VLLMPort)
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
	// defaults preserved
	if cfg.Inference.BundledModelID != "qwen2.5-coder-7b-instruct" {
		t.Errorf("MergeJSON corrupted defaults: %q", cfg.Inference.BundledModelID)
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
	// Other fields must be untouched.
	if cfg.Inference.IdleTimeout.Duration() != 10*time.Minute {
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

func TestExternalEndpoints_JSONParse(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/agent.json"
	body := `{
        "inference": {
            "external_endpoints": [
                {"id":"lan-vllm","url":"http://192.168.1.10:8000/v1","auth_env_var":"LAN_VLLM_KEY"},
                {"id":"openai","url":"https://api.openai.com/v1","auth_env_var":"OPENAI_API_KEY"},
                {"id":"future","url":"http://x:8000","disable":true}
            ]
        }
    }`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg := Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatalf("MergeJSON: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := cfg.Inference.ExternalEndpoints; len(got) != 3 {
		t.Fatalf("ExternalEndpoints len = %d, want 3", len(got))
	}
	if cfg.Inference.ExternalEndpoints[1].URL != "https://api.openai.com/v1" {
		t.Errorf("ExternalEndpoints[1].URL wrong: %+v", cfg.Inference.ExternalEndpoints[1])
	}
	if !cfg.Inference.ExternalEndpoints[2].Disable {
		t.Error("ExternalEndpoints[2] should be disabled")
	}
}

func TestExternalEndpoints_ValidateRejectsBadURL(t *testing.T) {
	cfg := Defaults()
	cfg.Inference.ExternalEndpoints = []ExternalEndpoint{{ID: "a", URL: "192.168.1.10:8000"}}
	if err := cfg.Validate(); err == nil {
		t.Error("bare host URL should be rejected (missing http://)")
	}
}

func TestExternalEndpoints_ValidateRejectsEmptyURL(t *testing.T) {
	cfg := Defaults()
	cfg.Inference.ExternalEndpoints = []ExternalEndpoint{{ID: "a"}}
	if err := cfg.Validate(); err == nil {
		t.Error("missing URL must be rejected")
	}
}

func TestExternalEndpoints_ValidateRejectsDuplicateIDs(t *testing.T) {
	cfg := Defaults()
	cfg.Inference.ExternalEndpoints = []ExternalEndpoint{
		{ID: "dupe", URL: "http://x:8000"},
		{ID: "dupe", URL: "http://y:8000"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("duplicate IDs must be rejected")
	}
}

func TestExternalEndpoints_ValidateRejectsTwoUnnamed(t *testing.T) {
	cfg := Defaults()
	cfg.Inference.ExternalEndpoints = []ExternalEndpoint{
		{URL: "http://a:8000"},
		{URL: "http://b:8000"},
	}
	if err := cfg.Validate(); err == nil {
		t.Error("two empty IDs must be rejected (would collide after host-default)")
	}
}

func TestExternalEndpoints_DefaultEmpty(t *testing.T) {
	cfg := Defaults()
	if len(cfg.Inference.ExternalEndpoints) != 0 {
		t.Errorf("Defaults must not seed ExternalEndpoints, got %v", cfg.Inference.ExternalEndpoints)
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
		name   string
		port   int
		source string
		want   int
	}{
		{"auto bundled", OllamaPortAuto, OllamaSourceBundled, DefaultOllamaBundledPort},
		{"auto empty source is bundled", OllamaPortAuto, "", DefaultOllamaBundledPort},
		{"auto reuse", OllamaPortAuto, OllamaSourceReuse, DefaultOllamaReusePort},
		// Every pre-existing agent.json serialized the old shared default
		// (11434) explicitly, so a literal 11434 under bundled is
		// indistinguishable from "never chose a port" — it flips to the
		// waired-owned port. "Bundled on 11434" is no longer expressible.
		{"legacy 11434 bundled flips", 11434, OllamaSourceBundled, DefaultOllamaBundledPort},
		{"legacy 11434 empty source flips", 11434, "", DefaultOllamaBundledPort},
		{"11434 reuse kept", 11434, OllamaSourceReuse, DefaultOllamaReusePort},
		{"explicit custom bundled kept", 8434, OllamaSourceBundled, 8434},
		{"explicit custom reuse kept", 21434, OllamaSourceReuse, 21434},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := InferenceConfig{OllamaPort: tc.port, OllamaSource: tc.source}
			if got := c.ResolvedOllamaPort(); got != tc.want {
				t.Errorf("ResolvedOllamaPort(port=%d, source=%q) = %d, want %d",
					tc.port, tc.source, got, tc.want)
			}
		})
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

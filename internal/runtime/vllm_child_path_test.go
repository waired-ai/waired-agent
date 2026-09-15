//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The vLLM child's PATH has to satisfy two things the engine needs at
// start-up, and both are load-bearing rather than conveniences:
//
//   - `ninja`, which flashinfer shells out to when it JIT-compiles CUDA
//     ops. It lives in the venv, and the spawned python never activates
//     the venv.
//   - `nvcc`, because vllm 0.28.0 stopped declaring flashinfer-cubin.
//     has_flashinfer() accepts the cubin package OR nvcc on PATH, so
//     without the first the engine's whole start-up rests on the second
//     (waired-agent#1133).
//
// Stated as properties of the resulting env rather than as a copy of
// what processEnv does, so a change that drops either argument fails
// here instead of being mirrored by the test.
func TestVLLMProcessEnv_PATHCarriesWhatStartupNeeds(t *testing.T) {
	a := &VLLMAdapter{cfg: VLLMConfig{Python: "/opt/venv/bin/python"}}
	var path string
	for _, kv := range a.processEnv() {
		if k, v, ok := strings.Cut(kv, "="); ok && k == "PATH" {
			path = v
		}
	}
	if path == "" {
		t.Fatal("child env has no PATH")
	}
	dirs := strings.Split(path, string(os.PathListSeparator))

	// The venv's bin dir, and first: a `ninja` on the host must not win
	// over the one the pin set installed.
	if dirs[0] != "/opt/venv/bin" {
		t.Errorf("PATH[0] = %q, want the venv bin dir %q", dirs[0], "/opt/venv/bin")
	}

	// If this host has an nvcc anywhere detectHostToolchain looks, the
	// child must be able to reach it — that is the whole point, and on a
	// host without CUDA there is nothing to assert and nothing to skip
	// around.
	nvcc := detectHostToolchain().NVCC
	if nvcc == "" {
		return
	}
	for _, d := range dirs {
		if d == "" {
			continue
		}
		if st, err := os.Stat(filepath.Join(d, "nvcc")); err == nil && !st.IsDir() {
			return
		}
	}
	t.Errorf("host has nvcc at %s but no PATH element of the vLLM child reaches one: %v", nvcc, dirs)
}

// An empty PATH element is read by execvp as the current directory, so a
// caller with nothing to contribute must not create one.
func TestVLLMProcessEnv_NoEmptyPATHElement(t *testing.T) {
	a := &VLLMAdapter{cfg: VLLMConfig{Python: "/opt/venv/bin/python"}}
	for _, kv := range a.processEnv() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k != "PATH" {
			continue
		}
		for _, d := range strings.Split(v, string(os.PathListSeparator)) {
			if d == "" {
				t.Fatalf("PATH has an empty element (reads as the cwd): %q", v)
			}
		}
	}
}

// lastEnv is the value exec gives the child for key: the last entry
// wins when a key repeats.
func lastEnv(env []string, key string) (string, bool) {
	val, found := "", false
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			val, found = v, true
		}
	}
	return val, found
}

// Under WSL2 vLLM turns pinned host memory off unless
// VLLM_WSL2_ENABLE_PIN_MEMORY=1 is set (vllm/platforms/cuda.py
// is_pin_memory_available). From 0.29.0 that is fatal rather than slow:
// the default model runner needs pinned memory and raises "UVA is not
// available" instead of falling back, so the engine never starts. A
// record of today's behaviour on the pinned release, measured on an RTX
// 5080 under WSL2 (Qwen3.5-2B bf16, fp8 KV).
//
// The variable is read only when vLLM detects WSL, so setting it on
// native Linux changes nothing.
func TestVLLMProcessEnv_EnablesPinnedMemoryUnderWSL2(t *testing.T) {
	t.Setenv("VLLM_WSL2_ENABLE_PIN_MEMORY", "")
	os.Unsetenv("VLLM_WSL2_ENABLE_PIN_MEMORY")
	a := &VLLMAdapter{cfg: VLLMConfig{Python: "/opt/venv/bin/python"}}
	if v, ok := lastEnv(a.processEnv(), "VLLM_WSL2_ENABLE_PIN_MEMORY"); !ok || v != "1" {
		t.Errorf("VLLM_WSL2_ENABLE_PIN_MEMORY = %q (set %v), want 1", v, ok)
	}
}

// An operator who set the variable themselves, in either direction,
// keeps their value, and ExtraEnv still has the last word.
func TestVLLMProcessEnv_PinnedMemoryYieldsToTheOperator(t *testing.T) {
	t.Setenv("VLLM_WSL2_ENABLE_PIN_MEMORY", "0")
	a := &VLLMAdapter{cfg: VLLMConfig{Python: "/opt/venv/bin/python"}}
	if v, _ := lastEnv(a.processEnv(), "VLLM_WSL2_ENABLE_PIN_MEMORY"); v != "0" {
		t.Errorf("inherited 0 was overridden: child sees %q", v)
	}

	t.Setenv("VLLM_WSL2_ENABLE_PIN_MEMORY", "")
	os.Unsetenv("VLLM_WSL2_ENABLE_PIN_MEMORY")
	a = &VLLMAdapter{cfg: VLLMConfig{Python: "/opt/venv/bin/python", ExtraEnv: []string{"VLLM_WSL2_ENABLE_PIN_MEMORY=0"}}}
	if v, _ := lastEnv(a.processEnv(), "VLLM_WSL2_ENABLE_PIN_MEMORY"); v != "0" {
		t.Errorf("ExtraEnv 0 was overridden: child sees %q", v)
	}
}

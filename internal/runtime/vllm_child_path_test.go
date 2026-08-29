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

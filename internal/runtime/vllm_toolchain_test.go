package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The host toolchain advisories (waired-agent#898).
//
// PRODUCT CONTRACT, ratified by waired-agent#898: a missing host
// compiler or CUDA toolkit is REPORTED, never a refusal — the venv is a
// valid ~6 GB artifact without them and an operator may be about to
// install one. The install reporting success while the engine could
// never start is the defect; the success itself is not.

func TestVLLMToolchainAdvisories(t *testing.T) {
	full := hostToolchain{CXX: "/usr/bin/g++", NVCC: "/usr/local/cuda/bin/nvcc", NVCCFrom: "/usr/local/cuda"}
	agree := bundledCUDA{NVCCVersion: "13.0", HeaderVersion: "13.0"}

	for _, tc := range []struct {
		name    string
		host    hostToolchain
		bundled bundledCUDA
		want    []string // substrings, one per expected advisory
	}{
		{"a complete host says nothing", full, agree, nil},
		{"no compiler", hostToolchain{NVCC: full.NVCC}, agree, []string{"g++"}},
		{"no cuda toolkit", hostToolchain{CXX: full.CXX}, agree, []string{"no CUDA toolkit"}},
		{"neither, compiler first", hostToolchain{}, agree, []string{"g++", "no CUDA toolkit"}},
		// The skew is the one this session actually hit, and it is
		// reported rather than fixed: the two wheels' version ranges
		// come from different dependencies of vllm itself.
		{"bundled cuda disagrees with itself", full,
			bundledCUDA{NVCCVersion: "13.2", HeaderVersion: "13.0"}, []string{"inconsistent"}},
		// "I could not read the bundle" must never be reported as "they
		// disagree" — the same fail-quiet rule the engine-version floors use.
		{"unreadable bundle is not a disagreement", full, bundledCUDA{}, nil},
		{"half-read bundle is not a disagreement", full, bundledCUDA{NVCCVersion: "13.2"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := vllmToolchainAdvisories(tc.host, tc.bundled)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d advisories, want %d:\n%v", len(got), len(tc.want), got)
			}
			for i, want := range tc.want {
				if !strings.Contains(got[i].Text, want) {
					t.Errorf("advisory %d = %q, want it to mention %q", i, got[i].Text, want)
				}
			}
		})
	}
}

// PRODUCT CONTRACT, ratified by waired-agent#957: an advisory blocks only
// when the engine will not start until it is fixed. The CLI puts the blocking
// ones under "This host cannot start the engine yet" and the rest under a
// heading that says the engine WILL start, so a mis-set flag here is a false
// statement on somebody's terminal — on a correctly provisioned host, which is
// where it was found.
func TestVLLMToolchainAdvisories_OnlyRealBlockersBlock(t *testing.T) {
	full := hostToolchain{CXX: "/usr/bin/g++", NVCC: "/usr/local/cuda/bin/nvcc"}
	skew := bundledCUDA{NVCCVersion: "13.2", HeaderVersion: "13.0"}

	// The case that produced #957: a complete host on the current pin set.
	// Its ONE advisory must not claim the engine cannot start — the advisory's
	// own text calls itself harmless here.
	only := vllmToolchainAdvisories(full, skew)
	if len(only) != 1 {
		t.Fatalf("complete host + skew: got %d advisories, want 1: %+v", len(only), only)
	}
	if only[0].Blocking {
		t.Errorf("the bundled-CUDA skew is marked blocking, so a host where the engine "+
			"starts fine is told it cannot: %q", only[0].Text)
	}

	// And the inverse, so "nothing blocks" cannot pass by marking nothing.
	for _, tc := range []struct {
		name string
		host hostToolchain
	}{
		{"no compiler", hostToolchain{NVCC: full.NVCC}},
		{"no cuda toolkit", hostToolchain{CXX: full.CXX}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := vllmToolchainAdvisories(tc.host, bundledCUDA{NVCCVersion: "13.0", HeaderVersion: "13.0"})
			if len(got) != 1 {
				t.Fatalf("got %d advisories, want 1: %+v", len(got), got)
			}
			if !got[0].Blocking {
				t.Errorf("%q is not marked blocking, but its own text says the engine "+
					"will not start: %q", tc.name, got[0].Text)
			}
		})
	}

	// Every advisory that SAYS the engine will not start must be flagged, and
	// nothing else may be. Keyed off the text so a new advisory cannot be
	// added with the wrong flag and stay unnoticed.
	for _, a := range vllmToolchainAdvisories(hostToolchain{}, skew) {
		says := strings.Contains(a.Text, "engine will not start")
		if says != a.Blocking {
			t.Errorf("Blocking=%v but the text %s say the engine will not start: %q",
				a.Blocking, map[bool]string{true: "does", false: "does not"}[says], a.Text)
		}
	}
}

// Every advisory has to name a way out, or it is only an alarm.
func TestVLLMToolchainAdvisories_EachNamesAnAction(t *testing.T) {
	got := vllmToolchainAdvisories(hostToolchain{}, bundledCUDA{NVCCVersion: "13.2", HeaderVersion: "13.0"})
	if len(got) != 3 {
		t.Fatalf("got %d advisories, want 3", len(got))
	}
	for i, a := range got {
		if !strings.Contains(a.Text, "apt-get install") && !strings.Contains(a.Text, "CUDA_HOME") {
			t.Errorf("advisory %d names no action: %q", i, a.Text)
		}
	}
}

// The skew advisory has to carry both versions and the trap, because
// the natural workaround for a missing toolkit is to point CUDA_HOME at
// the bundle — which is what produced this finding in the first place.
func TestVLLMToolchainAdvisories_SkewNamesBothVersionsAndTheTrap(t *testing.T) {
	got := vllmToolchainAdvisories(
		hostToolchain{CXX: "/usr/bin/g++", NVCC: "/usr/local/cuda/bin/nvcc"},
		bundledCUDA{NVCCVersion: "13.2", HeaderVersion: "13.0"})
	if len(got) != 1 {
		t.Fatalf("got %d advisories, want 1", len(got))
	}
	for _, want := range []string{"13.2", "13.0", "CUDA_HOME", "libcudart.so"} {
		if !strings.Contains(got[0].Text, want) {
			t.Errorf("skew advisory does not mention %q: %s", want, got[0].Text)
		}
	}
}

func TestParseNVCCVersion(t *testing.T) {
	const real = `nvcc: NVIDIA (R) Cuda compiler driver
Copyright (c) 2005-2025 NVIDIA Corporation
Built on Tue_Jan_21_00:00:00_PST_2025
Cuda compilation tools, release 13.2, V13.2.86
Build cuda_13.2.r13.2/compiler.37953736_0`
	if got := parseNVCCVersion(real); got != "13.2" {
		t.Errorf("parseNVCCVersion = %q, want 13.2", got)
	}
	for _, junk := range []string{"", "no version here", "release X.Y"} {
		if got := parseNVCCVersion(junk); got != "" {
			t.Errorf("parseNVCCVersion(%q) = %q, want empty", junk, got)
		}
	}
}

func TestParseCUDARTVersion(t *testing.T) {
	// CUDART_VERSION is major*1000 + minor*10, which is why this needs
	// arithmetic rather than a substring.
	for _, tc := range []struct{ in, want string }{
		{"#define CUDART_VERSION  13000", "13.0"},
		{"#define CUDART_VERSION 13020", "13.2"},
		{"#define CUDART_VERSION\t12080", "12.8"},
		{"nothing here", ""},
		{"#define CUDART_VERSION 0", ""},
	} {
		if got := parseCUDARTVersion(tc.in); got != tc.want {
			t.Errorf("parseCUDARTVersion(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestReadBundledCUDA(t *testing.T) {
	venv := t.TempDir()
	dir := filepath.Join(venv, "lib", "python3.12", "site-packages", "nvidia", "cu13")
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "include"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "nvcc"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "include", "cuda_runtime_api.h"),
		[]byte("#define CUDART_VERSION  13000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := readBundledCUDA(venv, func(string) string { return "Cuda compilation tools, release 13.2, V13.2.86" })
	if got.NVCCVersion != "13.2" || got.HeaderVersion != "13.0" {
		t.Errorf("readBundledCUDA = %+v, want {13.2 13.0}", got)
	}

	// RECORD OF TODAY'S BEHAVIOUR: a venv with no bundle at all reads as
	// zero values, which vllmToolchainAdvisories treats as "nothing to
	// say" rather than as a disagreement.
	if got := readBundledCUDA(t.TempDir(), func(string) string { return "release 13.2" }); got != (bundledCUDA{}) {
		t.Errorf("empty venv = %+v, want zero", got)
	}
}

func TestFormatAdvisories(t *testing.T) {
	got := formatAdvisories([]VLLMAdvisory{
		{Blocking: true, Text: "first"}, {Text: ""}, {Text: "  "}, {Text: "second"},
	})
	if len(got) != 2 {
		t.Fatalf("got %v, want 2 entries", got)
	}
	for _, g := range got {
		if !strings.HasPrefix(g, vllmAdvisoryPrefix) {
			t.Errorf("%q lacks the advisory prefix", g)
		}
	}
}

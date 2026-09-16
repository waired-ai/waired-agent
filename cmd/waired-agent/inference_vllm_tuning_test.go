package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/catalog/scoring"
	"github.com/waired-ai/waired-agent/internal/hardware"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

func vllmTuningFixture() (catalog.Manifest, catalog.Variant, hardware.Profile) {
	m := catalog.Manifest{ModelID: "gpt-oss-20b", ContextLength: 131072}
	v := catalog.Variant{
		VariantID:           "mxfp4-safetensors",
		EstimatedWeightGB:   14.0,
		KVBytesPerTokenFP16: 73728,
	}
	hw := hardware.Profile{
		GPUs: []hardware.GPU{{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034}},
	}
	return m, v, hw
}

func TestComputeVLLMTuning_ClampsBelowNative(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	// 1×L4 @ 0.85: ~18.1 GB budget (util×VRAM − per-GPU overhead −
	// activation reserve) − 14×1.15 GB weights → ~27k tokens (see
	// router.TestVLLMMaxModelLen).
	maxLen, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16)
	if maxLen != 26624 {
		t.Fatalf("maxLen = %d, want 26624", maxLen)
	}
	if mt.ContextLength != maxLen {
		t.Errorf("ModelTuning.ContextLength = %d, want %d", mt.ContextLength, maxLen)
	}
	if mt.ModelID != "gpt-oss-20b" || mt.VariantID != "mxfp4-safetensors" {
		t.Errorf("identity fields not filled: %+v", mt)
	}
	if !strings.Contains(mt.Warning, "clamped to 26624 tokens") ||
		!strings.Contains(mt.Warning, "131072") {
		t.Errorf("clamp warning should name both windows, got %q", mt.Warning)
	}
	// A 131072-native manifest used to be exempt from the coding-target
	// phrasing, because the native floor gate never admitted it. That gate
	// left with waired-ai/waired-agent#1400 (decisions 3 and 4 of
	// docs/decisions/20260916/0340): no such model ships, and a clamp below
	// the floor is named the same way for any manifest.
}

func TestComputeVLLMTuning_SubFloorClampNamesCodingTarget(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	m.ContextLength = 262144 // above the native floor → floor phrasing applies
	maxLen, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16)
	if maxLen != 26624 {
		t.Fatalf("maxLen = %d, want 26624", maxLen)
	}
	if !strings.Contains(mt.Warning, "~200k coding") {
		t.Errorf("expected the ~200k coding-target phrasing, got %q", mt.Warning)
	}
}

func TestComputeVLLMTuning_NoClampWhenBudgetCovers(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	// TP=2 doubles the budget past the 131072 native window.
	hw.GPUs = append(hw.GPUs, hw.GPUs[0])
	maxLen, mt := computeVLLMTuning(m, v, hw, 2, 0.85, scoring.KVFactorF16)
	if maxLen != m.ContextLength {
		t.Fatalf("maxLen = %d, want native %d", maxLen, m.ContextLength)
	}
	if mt.Warning != "" {
		t.Errorf("no warning expected when the native window fits, got %q", mt.Warning)
	}
	if mt.ContextLength != m.ContextLength {
		t.Errorf("ModelTuning.ContextLength = %d, want %d", mt.ContextLength, m.ContextLength)
	}
}

func TestComputeVLLMTuning_UnknownInputsPassThrough(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	v.KVBytesPerTokenFP16 = 0 // sizing unknown → never guess
	maxLen, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16)
	if maxLen != m.ContextLength {
		t.Fatalf("maxLen = %d, want manifest window %d", maxLen, m.ContextLength)
	}
	if mt.Warning != "" {
		t.Errorf("unknown inputs must not warn, got %q", mt.Warning)
	}
}

func TestComputeVLLMTuning_WeightsExceedBudgetWarns(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	v.EstimatedWeightGB = 40.0 // padded weights alone exceed a single L4
	maxLen, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16)
	if maxLen != m.ContextLength {
		t.Fatalf("maxLen = %d, want manifest window %d (no invented clamp)", maxLen, m.ContextLength)
	}
	if !strings.Contains(mt.Warning, "exceed") || !strings.Contains(mt.Warning, "engine.log") {
		t.Errorf("expected a weights-exceed-budget warning pointing at engine.log, got %q", mt.Warning)
	}
}

func TestComputeVLLMTuning_FP8DoublesTheClampedWindow(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	m.ContextLength = 262144
	f16, _ := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16)
	fp8, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorFP8)
	if fp8 <= f16 {
		t.Fatalf("fp8 window %d should exceed the f16 window %d", fp8, f16)
	}
	if fp8 != 54272 {
		t.Errorf("fp8 clamp = %d, want 54272 (halved KV → ~2× f16 26624)", fp8)
	}
	if mt.ContextLength != fp8 {
		t.Errorf("ModelTuning.ContextLength = %d, want %d", mt.ContextLength, fp8)
	}
}

func TestResolveVLLMKVCache(t *testing.T) {
	ada := hardware.Profile{GPUs: []hardware.GPU{
		{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034, ComputeCap: "8.9"},
	}}
	ampere := hardware.Profile{GPUs: []hardware.GPU{
		{Vendor: "nvidia", Model: "NVIDIA A100", VRAMTotalMB: 81920, ComputeCap: "8.0"},
	}}

	t.Run("Ada default-on selects fp8", func(t *testing.T) {
		dtype, factor := resolveVLLMKVCache(ada, false)
		if dtype != "fp8" || factor != scoring.KVFactorFP8 {
			t.Errorf("Ada: got (%q, %v), want (fp8, %v)", dtype, factor, scoring.KVFactorFP8)
		}
	})
	t.Run("Ada opt-out falls back to fp16", func(t *testing.T) {
		dtype, factor := resolveVLLMKVCache(ada, true)
		if dtype != "" || factor != scoring.KVFactorF16 {
			t.Errorf("Ada opt-out: got (%q, %v), want (\"\", %v)", dtype, factor, scoring.KVFactorF16)
		}
	})
	t.Run("Ampere never engages fp8 even without opt-out", func(t *testing.T) {
		dtype, factor := resolveVLLMKVCache(ampere, false)
		if dtype != "" || factor != scoring.KVFactorF16 {
			t.Errorf("Ampere: got (%q, %v), want (\"\", %v)", dtype, factor, scoring.KVFactorF16)
		}
	})
}

func TestVLLMKVCacheDType(t *testing.T) {
	if got := vllmKVCacheDType(true); got != "fp8" {
		t.Errorf("vllmKVCacheDType(true) = %q, want \"fp8\"", got)
	}
	if got := vllmKVCacheDType(false); got != "" {
		t.Errorf("vllmKVCacheDType(false) = %q, want \"\" (omit flag)", got)
	}
}

func TestVLLMSpeculativeConfigJSON(t *testing.T) {
	if got := vllmSpeculativeConfigJSON(false); got != "" {
		t.Errorf("disabled must omit the flag, got %q", got)
	}
	got := vllmSpeculativeConfigJSON(true)
	if !strings.Contains(got, `"method":"ngram"`) {
		t.Errorf("enabled config must select the ngram method, got %q", got)
	}
	if !json.Valid([]byte(got)) {
		t.Errorf("speculative config must be valid JSON, got %q", got)
	}
}

func TestParseVLLMKVCapacityTokens(t *testing.T) {
	cases := []struct {
		name string
		log  string
		want int
	}{
		{
			"v1 kv_cache_utils line with thousands separators",
			"INFO 07-05 12:00:01 [kv_cache_utils.py:1087] GPU KV cache size: 152,192 tokens\n" +
				"INFO 07-05 12:00:01 [kv_cache_utils.py:1091] Maximum concurrency for 131,072 tokens per request: 1.16x\n",
			152192,
		},
		{"plain number", "GPU KV cache size: 45056 tokens", 45056},
		{
			// No banner: a log written before #878, or an ollama one.
			// The whole file is one spawn, and the last line wins.
			"last occurrence wins in an unbannered log",
			"GPU KV cache size: 10,240 tokens\nGPU KV cache size: 20,480 tokens\n",
			20480,
		},
		{"absent", "vllm serving started\n", 0},
		{"garbage number", "GPU KV cache size: many tokens", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseVLLMKVCapacityTokens(tc.log); got != tc.want {
				t.Errorf("parseVLLMKVCapacityTokens = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestApplyVLLMTuningVerification(t *testing.T) {
	base := infruntime.ModelTuning{ModelID: "m", VariantID: "v", ContextLength: 45056}

	t.Run("capacity line marks verified", func(t *testing.T) {
		mt := applyVLLMTuningVerification(base, "GPU KV cache size: 80,000 tokens\n")
		if !mt.Verified {
			t.Errorf("expected Verified=true")
		}
		if mt.Warning != base.Warning {
			t.Errorf("warning must not change when capacity covers the window, got %q", mt.Warning)
		}
		// waired-agent#1126: the figure is kept, not only checked. It is
		// what says how many conversations this host holds warm.
		if mt.KVCapacityTokens != 80000 {
			t.Errorf("KVCapacityTokens = %d, want 80000", mt.KVCapacityTokens)
		}
	})

	t.Run("an inconclusive log leaves the pool unread", func(t *testing.T) {
		// 0 is what makes the vLLM host advertise the one-at-a-time
		// fail-safe rather than a slot count derived from nothing. The
		// line's wording and its value both belong to the pinned release.
		mt := applyVLLMTuningVerification(base, "no capacity line here\n")
		if mt.KVCapacityTokens != 0 {
			t.Errorf("KVCapacityTokens = %d, want 0", mt.KVCapacityTokens)
		}
	})

	t.Run("capacity below the window appends a warning", func(t *testing.T) {
		mt := applyVLLMTuningVerification(base, "GPU KV cache size: 40,960 tokens\n")
		if !mt.Verified {
			t.Errorf("expected Verified=true")
		}
		if !strings.Contains(mt.Warning, "40960") {
			t.Errorf("expected the measured capacity in the warning, got %q", mt.Warning)
		}
	})

	t.Run("absent line is inconclusive", func(t *testing.T) {
		mt := applyVLLMTuningVerification(base, "no capacity line here\n")
		if mt.Verified {
			t.Errorf("expected Verified=false on an inconclusive log")
		}
	})
}

// PRODUCT CONTRACT (waired-agent#885): the serve-flag gate fails closed.
// vLLM exits with argparse code 2 on an unrecognised flag and
// bootstrapVLLM swallows that into a log line, so "I could not read the
// version" must never be treated as "the version is fine". The floor is
// the pin itself because that is the one release whose flag set was read.
func TestVLLMServeFlagsSupported(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version string
		want    bool
	}{
		{"unreadable version fails closed", "", false},
		{"unparseable version fails closed", "not-a-version", false},
		{"venv older than the pin", "0.11.0", false},
		{"venv at the pin", infruntime.VLLMPinnedVersion, true},
		// Deliberately not a real release: this row was "0.25.1" and
		// stopped meaning "newer" the moment the pin moved past it
		// (waired-agent#1133). A version no upstream will ever publish
		// keeps the case testing what it names.
		{"venv newer than the pin", "999.0.0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := vllmServeFlagsSupported(tc.version); got != tc.want {
				t.Errorf("vllmServeFlagsSupported(%q) = %v, want %v", tc.version, got, tc.want)
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#887): a recognised failure names its
// cause and the setting to change; anything else says nothing at all.
// A wrong hint on a start-up failure is worse than none — it sends
// someone to change a setting that was never the problem.
func TestVLLMStartupDiagnosis(t *testing.T) {
	for _, tc := range []struct {
		name string
		log  string
		want string // substring
	}{
		{"unrecognised flag points at the venv",
			"usage: api_server [-h]\napi_server: error: unrecognized arguments: --kv-offloading-size 8\n",
			"runtimes install vllm"},
		// The same failure through `vllm serve` (the entry point since the
		// 0.29.0 pin), verbatim from a 0.29.0 venv: argparse names the
		// program main.py rather than api_server.
		{"unrecognised flag through vllm serve points at the venv",
			"usage: main.py [-h] [-v]\n               {chat,complete,serve,launch,bench,collect-env,run-batch} ...\nmain.py: error: unrecognized arguments: --nope\n",
			"runtimes install vllm"},
		{"no room for cache blocks points at the chunk first",
			"ValueError: No available memory for the cache blocks. Try increasing gpu_memory_utilization\n",
			"vllm_max_num_batched_tokens"},
		{"cuda oom points at the same pair",
			"torch.OutOfMemoryError: CUDA out of memory. Tried to allocate 2.00 GiB\n",
			"vllm_max_num_batched_tokens"},
		// The three signatures this lane walked through on real
		// hardware, in the order they appeared once each was fixed.
		{"no cuda toolkit points at the host, not the venv",
			"RuntimeError: Could not find nvcc and default cuda_home='/usr/local/cuda' doesn't exist\n",
			"cuda-toolkit"},
		{"no host c++ compiler",
			"gcc: fatal error: cannot execute 'cc1plus': posix_spawnp: No such file or directory\n",
			"g++"},
		{"a mismatched CUDA_HOME names the cause",
			"error: \"CUDA compiler and CUDA toolkit headers are incompatible, please check your include paths\"\n",
			"CUDA_HOME"},
		{"an unregistered parser points at its own key",
			"ValueError: invalid tool call parser: qwen9_xml (chose from ...)\n",
			"vllm_tool_parser"},
		// waired-agent#1026, verbatim from a host whose port was taken by
		// a container publishing 8000-8019. The API server binds before it
		// does anything else, so this OSError is the WHOLE log — there are
		// no vLLM diagnostics after it to recognise.
		{"a busy port names the address and the setting that moves it",
			"(APIServer pid=2581051)     sock.bind(addr)\n" +
				"(APIServer pid=2581051) OSError: [Errno 98] Address already in use\n",
			"127.0.0.1:9479"},
		{"an unrecognised failure stays silent", "Traceback (most recent call last):\n  RuntimeError: boom\n", ""},
		{"an empty log stays silent", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := vllmStartupDiagnosis(tc.log, "127.0.0.1:9479")
			if tc.want == "" {
				if got != "" {
					t.Errorf("guessed a cause it does not know: %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("diagnosis = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// vllmSpawnBanner is the delimiter internal/runtime writes before each
// vLLM spawn (waired-agent#878). It is spelled out here rather than
// exported from that package on purpose: these tests are the reader's
// side of a format contract, and a literal is what makes them fail if
// the writer's format moves. They fail loudly — LastEngineLogSpawn falls
// back to the whole log when it finds no banner, so the scoping cases
// below return the earlier spawn's figures instead of ignoring them.
func vllmSpawnBanner(ts string) string {
	return "\n===== waired: vllm spawn " + ts + " =====\n"
}

// PRODUCT CONTRACT (waired-agent#878): the KV capacity that marks a
// tuning Verified is the one the RUNNING engine reported. engine.log now
// accumulates spawns, and an earlier spawn's figure was measured under
// sizing that is no longer loaded — reporting it would present a stale
// number as a measurement.
func TestParseVLLMKVCapacityTokens_ScopedToTheRunningSpawn(t *testing.T) {
	for _, tc := range []struct {
		name string
		log  string
		want int
	}{
		{
			"the current spawn's figure wins over an earlier one",
			vllmSpawnBanner("2026-08-21T00:00:00Z") + "GPU KV cache size: 10,240 tokens\n" +
				vllmSpawnBanner("2026-08-21T00:01:00Z") + "GPU KV cache size: 152,192 tokens\n",
			152192,
		},
		{
			// The mutation that matters: drop the scoping and this
			// returns 10,240 — a pool that is not loaded any more,
			// reported as this engine's verified capacity.
			"a spawn that reported no capacity is inconclusive, not the previous one's",
			vllmSpawnBanner("2026-08-21T00:00:00Z") + "GPU KV cache size: 10,240 tokens\n" +
				vllmSpawnBanner("2026-08-21T00:01:00Z") + "ValueError: No available memory for the cache blocks\n",
			0,
		},
		{
			"a failed first attempt does not hide the successful second",
			vllmSpawnBanner("2026-08-21T00:00:00Z") + "error: unrecognized arguments: --nope\n" +
				vllmSpawnBanner("2026-08-21T00:01:00Z") + "GPU KV cache size: 45,056 tokens\n",
			45056,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseVLLMKVCapacityTokens(tc.log); got != tc.want {
				t.Errorf("parseVLLMKVCapacityTokens = %d, want %d", got, tc.want)
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#878): the hint the bootstrap logs names
// the cause of the attempt the retry loop ended on. Scanning the whole
// file would let whichever arm of vllmStartupDiagnosis matches first win
// regardless of which attempt it came from — so a transient first
// failure would outrank the reason the loop actually gave up.
func TestVLLMStartupHint_DiagnosesTheAttemptTheLoopEndedOn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		log     string
		want    string // substring, "" = silent
		notWant string
	}{
		{
			name: "the last attempt's cause, not the first's",
			log: vllmSpawnBanner("2026-08-21T00:00:00Z") + "api_server: error: unrecognized arguments: --nope\n" +
				vllmSpawnBanner("2026-08-21T00:00:10Z") + "torch.OutOfMemoryError: CUDA out of memory\n",
			want:    "vllm_max_num_batched_tokens",
			notWant: "runtimes install vllm",
		},
		{
			name: "an attempt that recognises nothing stays silent even when an earlier one did not",
			log: vllmSpawnBanner("2026-08-21T00:00:00Z") + "api_server: error: unrecognized arguments: --nope\n" +
				vllmSpawnBanner("2026-08-21T00:00:10Z") + "Traceback (most recent call last):\n  RuntimeError: boom\n",
			want: "",
		},
		{
			// Back-compat: a host that has not respawned since the
			// upgrade still has an unbannered log, and it must still
			// be diagnosed.
			name: "an unbannered log is diagnosed whole",
			log:  "api_server: error: unrecognized arguments: --kv-offloading-size 8\n",
			want: "runtimes install vllm",
		},
		{
			// waired-agent#1026: every attempt of a busy-port failure
			// fails identically, which is exactly the shape the loop
			// ends on. The hint must name inference.vllm_port, not the
			// port that was free two attempts ago.
			name: "a busy port survives the per-spawn scoping",
			log: vllmSpawnBanner("2026-08-27T00:00:00Z") + "OSError: [Errno 98] Address already in use\n" +
				vllmSpawnBanner("2026-08-27T00:00:10Z") + "OSError: [Errno 98] Address already in use\n",
			want: "inference.vllm_port",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := vllmStartupHint(tc.log, 9479)
			if tc.want == "" {
				if got != "" {
					t.Errorf("guessed a cause it does not know: %q", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("hint = %q, want it to mention %q", got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("hint = %q, want it NOT to mention %q — that was an earlier attempt", got, tc.notWant)
			}
		})
	}
}

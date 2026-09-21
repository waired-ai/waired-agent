package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/hardware"
	"github.com/waired-ai/waired-agent/internal/router"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
	"github.com/waired-ai/waired-agent/proto/catalog/scoring"
)

func vllmTuningFixture() (catalog.Manifest, catalog.Variant, hardware.Profile) {
	m := catalog.Manifest{ModelID: "big-model", ContextLength: 262144}
	v := catalog.Variant{
		VariantID:           "awq-safetensors",
		EstimatedWeightGB:   14.0,
		KVBytesPerTokenFP16: 73728,
	}
	hw := hardware.Profile{
		GPUs: []hardware.GPU{{Vendor: "nvidia", Model: "NVIDIA L4", VRAMTotalMB: 23034}},
	}
	return m, v, hw
}

// vLLM serves one of the two tiers and nothing between them, or — when the
// KV pool cannot hold 200,704 — marks the build as one this host does not
// serve.
//
// PRODUCT CONTRACT, ratifying source: owner decision 2026-09-16 on
// waired-agent#1396 (engines serve only 200,704 or 1,048,576), implemented
// by #1434. It inverts #675's clamp to any window the pool fits and the
// native window whenever the pool covered it.

func TestComputeVLLMTuning_BelowTheTierIsNotServed(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	// 1×L4 @ 0.85: ~18.1 GB budget − 14×1.15 GB weights → ~27k tokens (see
	// router.TestVLLMMaxModelLen).
	maxLen, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16, router.VLLMSpeculation{})
	if maxLen != 26624 {
		t.Fatalf("maxLen = %d, want the pool's 26624", maxLen)
	}
	if mt.ContextLength != maxLen || mt.WindowFits {
		t.Errorf("ContextLength/WindowFits = %d/%v, want %d/false", mt.ContextLength, mt.WindowFits, maxLen)
	}
	if mt.ModelID != "big-model" || mt.VariantID != "awq-safetensors" {
		t.Errorf("identity fields not filled: %+v", mt)
	}
	if want := fmt.Sprintf(vllmBelowTierWarning, 26624, 0.85, 1); mt.Warning != want {
		t.Errorf("warning = %q\nwant      %q", mt.Warning, want)
	}
}

func TestComputeVLLMTuning_ServesThe200kTierNotTheNativeWindow(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	// TP=2 doubles the budget past the 262144 native window, which used to
	// be served as it was.
	hw.GPUs = append(hw.GPUs, hw.GPUs[0])
	maxLen, mt := computeVLLMTuning(m, v, hw, 2, 0.85, scoring.KVFactorF16, router.VLLMSpeculation{})
	if maxLen != 200704 {
		t.Fatalf("maxLen = %d, want the 200k tier", maxLen)
	}
	if mt.Warning != "" || !mt.WindowFits || mt.ContextLength != 200704 {
		t.Errorf("tuning = %+v, want 200704, fits, no warning", mt)
	}
}

func TestComputeVLLMTuning_Serves1MOnlyWhenThePoolHoldsIt(t *testing.T) {
	m := catalog.Manifest{ModelID: "long-model", ContextLength: 1 << 20}
	v := catalog.Variant{VariantID: "fp8", EstimatedWeightGB: 8.0, KVBytesPerTokenFP16: 65536}
	for _, tc := range []struct {
		name string
		gpus int
		vram int
		want int
	}{
		{"a pool between the tiers serves 200k", 1, 24463, 200704},
		{"a pool past 1M serves 1M", 4, 81920, 1 << 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hw hardware.Profile
			for i := 0; i < tc.gpus; i++ {
				hw.GPUs = append(hw.GPUs, hardware.GPU{Vendor: "nvidia", VRAMTotalMB: tc.vram, ComputeCap: "12.0"})
			}
			est := router.VLLMMaxModelLenFor(v, 0, tc.gpus, 0.85, scoring.KVFactorFP8, hw)
			maxLen, mt := computeVLLMTuning(m, v, hw, tc.gpus, 0.85, scoring.KVFactorFP8, router.VLLMSpeculation{})
			if maxLen != tc.want || mt.ContextLength != tc.want || !mt.WindowFits {
				t.Errorf("maxLen = %d (pool estimate %d), tuning %+v, want %d", maxLen, est, mt, tc.want)
			}
		})
	}
}

func TestComputeVLLMTuning_UnknownInputsServeTheTier(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	v.KVBytesPerTokenFP16 = 0 // sizing unknown → never guess upward
	maxLen, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16, router.VLLMSpeculation{})
	if maxLen != 200704 {
		t.Fatalf("maxLen = %d, want 200704, not the native %d", maxLen, m.ContextLength)
	}
	if mt.Warning != "" {
		t.Errorf("unknown inputs must not warn, got %q", mt.Warning)
	}
	// CI's internal_only model is the one exception: its own window.
	small := catalog.Manifest{ModelID: "tiny", ContextLength: 32768}
	if got, _ := computeVLLMTuning(small, v, hw, 1, 0.85, scoring.KVFactorF16, router.VLLMSpeculation{}); got != 32768 {
		t.Errorf("a 32k model's unsized window = %d, want its own 32768", got)
	}
}

func TestComputeVLLMTuning_WeightsExceedBudgetWarns(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	v.EstimatedWeightGB = 40.0 // padded weights alone exceed a single L4
	maxLen, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16, router.VLLMSpeculation{})
	if maxLen != 200704 {
		t.Fatalf("maxLen = %d, want the 200k tier (no invented clamp)", maxLen)
	}
	if !strings.Contains(mt.Warning, "exceed") || !strings.Contains(mt.Warning, "engine.log") {
		t.Errorf("expected a weights-exceed-budget warning pointing at engine.log, got %q", mt.Warning)
	}
}

func TestComputeVLLMTuning_FP8DoublesThePool(t *testing.T) {
	m, v, hw := vllmTuningFixture()
	f16, _ := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorF16, router.VLLMSpeculation{})
	fp8, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorFP8, router.VLLMSpeculation{})
	if fp8 <= f16 {
		t.Fatalf("fp8 window %d should exceed the f16 window %d", fp8, f16)
	}
	if fp8 != 54272 {
		t.Errorf("fp8 pool = %d, want 54272 (halved KV → ~2× f16 26624)", fp8)
	}
	if mt.ContextLength != fp8 || mt.WindowFits {
		t.Errorf("ModelTuning = %+v, want %d and not fitting the tier", mt, fp8)
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

// An MTP draft is sized into the pool and recorded on the tuning
// (waired-ai/waired#1432); the window served is still a tier. The draft's
// effect on the pool estimate itself is pinned in the router's tests.
func TestComputeVLLMTuning_SizesTheMTPDraft(t *testing.T) {
	m := catalog.Manifest{ModelID: "qwen3.5-4b", ContextLength: 1 << 20}
	v := catalog.Variant{VariantID: "bf16", EstimatedWeightGB: 8.5, KVBytesPerTokenFP16: 32768,
		MTPLayers: 1, MTPKVBytesPerTokenFP16: 4096, MTPDraftTokens: 1}
	hw := hardware.Profile{GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24463, ComputeCap: "12.0"}}}

	none, plain := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorFP8, router.VLLMSpeculation{})
	legacy := router.VLLMMaxModelLen(v.EstimatedWeightGB, v.KVBytesPerTokenFP16, 1, 0.85, scoring.KVFactorFP8, hw)
	if want := vllmTierWindow(m, legacy); none != want {
		t.Errorf("no draft: max_model_len %d, want the tier %d of the draft-free estimate %d", none, want, legacy)
	}
	if plain.SpeculativeMethod != "" || plain.SpeculativeTokens != 0 {
		t.Errorf("no draft recorded as %q/%d", plain.SpeculativeMethod, plain.SpeculativeTokens)
	}
	spec := router.VLLMSpeculative(v, false, false, true)
	withMTP, mt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorFP8, spec)
	drafted := router.VLLMMaxModelLenFor(v, spec.DraftTokens(), 1, 0.85, scoring.KVFactorFP8, hw)
	if drafted >= legacy {
		t.Errorf("MTP draft: pool estimate %d, want below the draft-free %d", drafted, legacy)
	}
	if want := vllmTierWindow(m, drafted); withMTP != want {
		t.Errorf("MTP draft: max_model_len %d, want the tier %d of the drafted estimate %d", withMTP, want, drafted)
	}
	if mt.SpeculativeMethod != "mtp" || mt.SpeculativeTokens != 1 {
		t.Errorf("MTP recorded as %q/%d, want mtp/1", mt.SpeculativeMethod, mt.SpeculativeTokens)
	}
	ngram, nt := computeVLLMTuning(m, v, hw, 1, 0.85, scoring.KVFactorFP8, router.VLLMSpeculative(v, true, false, true))
	if ngram != none || nt.SpeculativeMethod != "ngram" {
		t.Errorf("ngram: max_model_len %d method %q, want the draft-free %d and ngram recorded", ngram, nt.SpeculativeMethod, none)
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

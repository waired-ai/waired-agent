package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
)

func TestPickEngine_NoGPU_PicksOllama(t *testing.T) {
	hw := hardware.Profile{RAMTotalGB: 16, GPUs: nil}
	pick, err := PickEngine(EnginePickInput{Hardware: hw})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	if pick.Engine != "ollama" {
		t.Errorf("Engine = %q, want ollama (no GPU detected)", pick.Engine)
	}
	if pick.Source != EngineSourceAuto {
		t.Errorf("Source = %q, want auto", pick.Source)
	}
}

// setVLLMAutoSelectable flips the #557 auto-selection gate for a test and
// restores it afterwards.
func setVLLMAutoSelectable(t *testing.T, v bool) {
	t.Helper()
	old := VLLMAutoSelectable
	VLLMAutoSelectable = v
	t.Cleanup(func() { VLLMAutoSelectable = old })
}

// With the auto-picker gated off (VLLMAutoSelectable=false — an operator/build
// opt-out, no longer the default since #557 landed), a large NVIDIA host
// auto-picks ollama and says why. Pins that the gate still works.
func TestPickEngine_NVIDIASufficientVRAM_AutoOllamaWhenGatedOff(t *testing.T) {
	setVLLMAutoSelectable(t, false)
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	pick, err := PickEngine(EnginePickInput{Hardware: hw})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	if pick.Engine != "ollama" {
		t.Errorf("Engine = %q, want ollama (vllm serving unwired, #557)", pick.Engine)
	}
	found557 := false
	for _, r := range pick.Reasons {
		if strings.Contains(r, "#557") {
			found557 = true
			break
		}
	}
	if !found557 {
		t.Errorf("Reasons should explain the #557 vllm gate; got %v", pick.Reasons)
	}
}

// Once vLLM serving is wired (VLLMAutoSelectable=true), the same large NVIDIA
// host auto-picks vllm. This guards the branch that the #557 gate currently
// short-circuits. OS is set explicitly because vLLM serving is Linux-only
// (VLLMSupportedOS) — an unset Profile.OS fails closed to ollama.
func TestPickEngine_NVIDIASufficientVRAM_PicksVLLMWhenWired(t *testing.T) {
	setVLLMAutoSelectable(t, true)
	hw := hardware.Profile{
		OS:         "linux",
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	pick, err := PickEngine(EnginePickInput{Hardware: hw})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	if pick.Engine != "vllm" {
		t.Errorf("Engine = %q, want vllm (NVIDIA GPU + 24GB VRAM, vllm wired)", pick.Engine)
	}
}

func TestPickEngine_NVIDIASmallVRAM_FallsToOllama(t *testing.T) {
	// 4 GB GPU — below the 8 GB threshold for vLLM serving.
	hw := hardware.Profile{
		RAMTotalGB: 16,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 4096}},
	}
	pick, err := PickEngine(EnginePickInput{Hardware: hw})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	if pick.Engine != "ollama" {
		t.Errorf("Engine = %q, want ollama (4 GB GPU < 8 GB threshold)", pick.Engine)
	}
}

// A GPU detected without a VRAM figure is the shape the #67 fallbacks
// can produce: the Linux procfs inventory names the card but carries no
// capacity, and a Windows driver may publish no registry memory value.
// Product contract: an unknown budget must not be spent as if it were a
// large one — vLLM needs a real 8 GB+ figure, so the pick stays on
// ollama, which degrades to CPU offload on its own if the card cannot
// hold the model.
func TestPickEngine_NVIDIAUnknownVRAM_FallsToOllama(t *testing.T) {
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "NVIDIA GeForce RTX 3060 Ti"}},
	}
	pick, err := PickEngine(EnginePickInput{Hardware: hw})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	if pick.Engine != "ollama" {
		t.Errorf("Engine = %q, want ollama (VRAM unknown is not a vLLM budget)", pick.Engine)
	}
}

func TestPickEngine_NonNVIDIAVendor_FallsToOllama(t *testing.T) {
	// AMD GPU detected, but the vLLM ROCm runtime adapter is not wired
	// yet — picker must route to Ollama (working ROCm/Vulkan path).
	hw := hardware.Profile{
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "amd", Model: "MI250", VRAMTotalMB: 64000}},
	}
	pick, err := PickEngine(EnginePickInput{Hardware: hw})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	if pick.Engine != "ollama" {
		t.Errorf("Engine = %q, want ollama (AMD GPU, vLLM adapter unimplemented)", pick.Engine)
	}
	// AMD-specific reason must be present so operators understand why
	// vLLM was skipped despite the GPU being large enough.
	foundAMD := false
	for _, r := range pick.Reasons {
		if strings.Contains(r, "AMD") {
			foundAMD = true
			break
		}
	}
	if !foundAMD {
		t.Errorf("Reasons should mention AMD-specific fallback; got %v", pick.Reasons)
	}
}

func TestPickEngine_AppleSilicon_FallsToOllama(t *testing.T) {
	// UMA host (Apple Silicon) — route to Ollama Metal. MLX runtime is
	// scope-out for this PR.
	hw := hardware.Profile{
		RAMTotalGB:    192,
		UnifiedMemory: true,
		UsableVRAMMB:  144 * 1024,
		GPUs:          []hardware.GPU{{Vendor: "apple", Model: "Apple M4 Ultra"}},
	}
	pick, err := PickEngine(EnginePickInput{Hardware: hw})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	if pick.Engine != "ollama" {
		t.Errorf("Engine = %q, want ollama (Apple Silicon, MLX adapter unimplemented)", pick.Engine)
	}
	foundApple := false
	for _, r := range pick.Reasons {
		if strings.Contains(r, "Apple") {
			foundApple = true
			break
		}
	}
	if !foundApple {
		t.Errorf("Reasons should mention Apple-specific fallback; got %v", pick.Reasons)
	}
}

// TestPickEngine_AppleSilicon_SmallerSKU covers the M-class entry-tier
// (M-series Pro/Max with 16 GB UMA) as well — the picker must still
// route to Ollama Metal regardless of UMA budget because no MLX-LM
// adapter is wired yet. The 8 GB vLLM threshold should not push the
// pick toward vllm even when EffectiveVRAMMB exceeds it, because the
// apple branch is taken first.
func TestPickEngine_AppleSilicon_SmallerSKU(t *testing.T) {
	hw := hardware.Profile{
		RAMTotalGB:    16,
		UnifiedMemory: true,
		UsableVRAMMB:  12 * 1024, // 75 % of 16 GB
		GPUs:          []hardware.GPU{{Vendor: "apple", Model: "Apple M3"}},
	}
	pick, err := PickEngine(EnginePickInput{Hardware: hw})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	if pick.Engine != "ollama" {
		t.Errorf("Engine = %q, want ollama (Apple M3, MLX adapter unimplemented)", pick.Engine)
	}
}

func TestPickEngine_NVIDIA_UnifiedMemoryDoesNotApply(t *testing.T) {
	// vllm must be reachable for this test to discriminate the VRAM source:
	// the #557 gate would otherwise route every NVIDIA host to ollama.
	setVLLMAutoSelectable(t, true)
	// NVIDIA discrete GPU on a host that (incorrectly) reports UMA
	// must still consult GPUs[0].VRAMTotalMB rather than UsableVRAMMB.
	hw := hardware.Profile{
		OS:            "linux", // vllm is Linux-only; see VLLMSupportedOS
		RAMTotalGB:    64,
		UnifiedMemory: false, // discrete-GPU host
		UsableVRAMMB:  0,
		GPUs:          []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24000}},
	}
	pick, err := PickEngine(EnginePickInput{Hardware: hw})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	if pick.Engine != "vllm" {
		t.Errorf("Engine = %q, want vllm (NVIDIA 24 GB discrete)", pick.Engine)
	}
}

func TestPickEngine_PreferenceForcesOllama(t *testing.T) {
	hw := hardware.Profile{
		RAMTotalGB: 64,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
	}
	pick, err := PickEngine(EnginePickInput{Hardware: hw, Preference: "ollama"})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	if pick.Engine != "ollama" {
		t.Errorf("Engine = %q, want ollama (preference override)", pick.Engine)
	}
	if pick.Source != EngineSourcePreference {
		t.Errorf("Source = %q, want preference", pick.Source)
	}
}

func TestPickEngine_PreferenceForcesVLLM_NoGPU(t *testing.T) {
	// Preferring vllm on a host with no GPU should still honour the
	// preference (the operator knows what they're asking for); the
	// adapter will simply fail to start later. This is an explicit
	// choice — we don't second-guess --prefer.
	hw := hardware.Profile{RAMTotalGB: 16, GPUs: nil}
	pick, err := PickEngine(EnginePickInput{Hardware: hw, Preference: "vllm"})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	if pick.Engine != "vllm" {
		t.Errorf("Engine = %q, want vllm (preference honoured even without GPU)", pick.Engine)
	}
	if pick.Source != EngineSourcePreference {
		t.Errorf("Source = %q, want preference", pick.Source)
	}
}

func TestPickEngine_InvalidPreference(t *testing.T) {
	hw := hardware.Profile{RAMTotalGB: 16, GPUs: nil}
	_, err := PickEngine(EnginePickInput{Hardware: hw, Preference: "tensorrt"})
	if !errors.Is(err, ErrInvalidEnginePreference) {
		t.Errorf("err = %v, want ErrInvalidEnginePreference", err)
	}
}

// Product contract (waired-agent#319): vLLM serving exists on Linux only.
// The Windows/darwin installers are stubs whose Active() always answers
// "no install", so the auto-picker must never advertise vllm there — and an
// unknown (empty) OS fails closed to the same answer.
func TestVLLMSupportedOS(t *testing.T) {
	cases := []struct {
		goos string
		want bool
	}{
		{"linux", true},
		{"windows", false},
		{"darwin", false},
		{"", false}, // unpopulated Profile.OS must not unlock vllm
	}
	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			if got := VLLMSupportedOS(tc.goos); got != tc.want {
				t.Errorf("VLLMSupportedOS(%q) = %v, want %v", tc.goos, got, tc.want)
			}
		})
	}
}

// VLLMAutoEligible is the one rule PickEngine and the CLI's recommendEngine
// both answer through. Product contract: all four terms must hold — the #557
// gate, an NVIDIA GPU, MinVLLMVRAMMB of VRAM, and a Linux host.
func TestVLLMAutoEligible(t *testing.T) {
	cases := []struct {
		name       string
		selectable bool
		goos       string
		vendor     string
		vramMB     int
		want       bool
	}{
		{"linux nvidia large", true, "linux", "nvidia", 24467, true},
		{"linux nvidia mixed case vendor", true, "linux", "NVIDIA", 24467, true},
		{"linux nvidia at threshold", true, "linux", "nvidia", MinVLLMVRAMMB, true},
		{"linux nvidia below threshold", true, "linux", "nvidia", MinVLLMVRAMMB - 1, false},
		{"windows nvidia large", true, "windows", "nvidia", 24467, false},
		{"darwin nvidia large", true, "darwin", "nvidia", 24467, false},
		{"unknown os nvidia large", true, "", "nvidia", 24467, false},
		{"linux amd large", true, "linux", "amd", 64000, false},
		{"linux apple large", true, "linux", "apple", 64000, false},
		{"gated off", false, "linux", "nvidia", 24467, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setVLLMAutoSelectable(t, tc.selectable)
			if got := VLLMAutoEligible(tc.goos, tc.vendor, tc.vramMB); got != tc.want {
				t.Errorf("VLLMAutoEligible(%q, %q, %d) = %v, want %v",
					tc.goos, tc.vendor, tc.vramMB, got, tc.want)
			}
		})
	}
}

// The regression this fixes (waired-agent#319): a Windows host with a 16 GB
// RTX 5080 auto-picked vllm, so every catalog surface judged families against
// an engine the host cannot serve while the daemon actually ran ollama.
// Product contract: the same hardware answers ollama on Windows and darwin,
// vllm only on Linux, and the reason names the OS gate so the audit trail
// explains itself.
func TestPickEngine_NVIDIALargeVRAM_GatedByOS(t *testing.T) {
	cases := []struct {
		goos       string
		wantEngine string
	}{
		{"linux", "vllm"},
		{"windows", "ollama"},
		{"darwin", "ollama"},
		{"", "ollama"}, // unknown OS fails closed
	}
	for _, tc := range cases {
		name := tc.goos
		if name == "" {
			name = "unknown"
		}
		t.Run(name, func(t *testing.T) {
			setVLLMAutoSelectable(t, true)
			hw := hardware.Profile{
				OS:         tc.goos,
				RAMTotalGB: 32,
				GPUs:       []hardware.GPU{{Vendor: "nvidia", Model: "RTX 5080", VRAMTotalMB: 16303}},
			}
			pick, err := PickEngine(EnginePickInput{Hardware: hw})
			if err != nil {
				t.Fatalf("PickEngine: %v", err)
			}
			if pick.Engine != tc.wantEngine {
				t.Fatalf("Engine = %q, want %q (os=%q)", pick.Engine, tc.wantEngine, tc.goos)
			}
			if tc.wantEngine != "ollama" {
				return
			}
			found := false
			for _, r := range pick.Reasons {
				if strings.Contains(r, "linux-only") {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Reasons should name the linux-only vllm gate; got %v", pick.Reasons)
			}
		})
	}
}

// The OS gate is an AUTO-picker rule only. An operator who asks for vllm on
// Windows still gets it: `--prefer vllm` means "I have a reason", and the
// serving path's engineViable is where that decision is actually enforced.
func TestPickEngine_PreferenceForcesVLLM_OnNonLinux(t *testing.T) {
	hw := hardware.Profile{
		OS:         "windows",
		RAMTotalGB: 32,
		GPUs:       []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 16303}},
	}
	pick, err := PickEngine(EnginePickInput{Hardware: hw, Preference: "vllm"})
	if err != nil {
		t.Fatalf("PickEngine: %v", err)
	}
	if pick.Engine != "vllm" {
		t.Errorf("Engine = %q, want vllm (preference wins over the OS gate)", pick.Engine)
	}
	if pick.Source != EngineSourcePreference {
		t.Errorf("Source = %q, want preference", pick.Source)
	}
}

func TestPickEngine_Reasons(t *testing.T) {
	cases := []struct {
		name string
		in   EnginePickInput
	}{
		{"auto-nvidia", EnginePickInput{
			Hardware: hardware.Profile{
				GPUs: []hardware.GPU{{Vendor: "nvidia", VRAMTotalMB: 24467}},
			},
		}},
		{"auto-ollama", EnginePickInput{
			Hardware: hardware.Profile{RAMTotalGB: 16},
		}},
		{"preference", EnginePickInput{
			Hardware:   hardware.Profile{RAMTotalGB: 16},
			Preference: "vllm",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pick, err := PickEngine(tc.in)
			if err != nil {
				t.Fatalf("PickEngine: %v", err)
			}
			if len(pick.Reasons) == 0 {
				t.Errorf("Reasons must be non-empty for audit trail")
			}
		})
	}
}

package main

import (
	"bufio"
	"bytes"
	"flag"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/hardware"
)

func boolPtr(b bool) *bool { return &b }

// newTestFlagSet returns a fresh FlagSet with error output suppressed
// so a bad parse doesn't pollute test output.
func newTestFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	return fs
}

// gpuProfile returns a Profile with one discrete NVIDIA GPU.
func gpuProfile(vramMB int) hardware.Profile {
	return hardware.Profile{
		GPUs: []hardware.GPU{{Vendor: "nvidia", Model: "RTX 4060", VRAMTotalMB: vramMB}},
	}
}

func umaProfile(usableMB int) hardware.Profile {
	return hardware.Profile{
		GPUs:          []hardware.GPU{{Vendor: "apple", Model: "M2"}},
		UnifiedMemory: true,
		UsableVRAMMB:  usableMB,
	}
}

func cpuProfile() hardware.Profile {
	return hardware.Profile{RAMTotalGB: 16}
}

func TestPromptInference_FlagOverridesWin(t *testing.T) {
	// Both flags forced, hardware says no — flags must win, and no
	// stdin should be consumed (the bytes.Buffer below is empty so a
	// Scan() would return false anyway, but assert we never touched it).
	out := &bytes.Buffer{}
	in := strings.NewReader("")
	got := promptInference(in, out,
		agentconfig.InferenceConfig{}, false,
		cpuProfile(),
		boolPtr(true), boolPtr(false),
		false)
	if !got.Enabled || got.ShareWithMesh {
		t.Errorf("got %+v, want Enabled=true ShareWithMesh=false", got)
	}
	if out.Len() != 0 {
		t.Errorf("expected no prompt output, got %q", out.String())
	}
}

func TestPromptInference_NonInteractive_LargeVRAM(t *testing.T) {
	got := promptInference(strings.NewReader(""), &bytes.Buffer{},
		agentconfig.InferenceConfig{}, false,
		gpuProfile(12288),
		nil, nil,
		true)
	if !got.Enabled || !got.ShareWithMesh {
		t.Errorf("got %+v, want Enabled=true ShareWithMesh=true", got)
	}
}

func TestPromptInference_NonInteractive_CPUOnly(t *testing.T) {
	got := promptInference(strings.NewReader(""), &bytes.Buffer{},
		agentconfig.InferenceConfig{}, false,
		cpuProfile(),
		nil, nil,
		true)
	if got.Enabled || got.ShareWithMesh {
		t.Errorf("got %+v, want Enabled=false ShareWithMesh=false", got)
	}
}

func TestPromptInference_NonInteractive_AppleSilicon(t *testing.T) {
	got := promptInference(strings.NewReader(""), &bytes.Buffer{},
		agentconfig.InferenceConfig{}, false,
		umaProfile(16384),
		nil, nil,
		true)
	if !got.Enabled || !got.ShareWithMesh {
		t.Errorf("got %+v, want Enabled=true ShareWithMesh=true", got)
	}
}

func TestPromptInference_NonInteractive_LowVRAM_iGPU(t *testing.T) {
	got := promptInference(strings.NewReader(""), &bytes.Buffer{},
		agentconfig.InferenceConfig{}, false,
		gpuProfile(2048), // 2 GB iGPU — under the 8 GB threshold.
		nil, nil,
		true)
	if got.Enabled || got.ShareWithMesh {
		t.Errorf("got %+v, want Enabled=false (iGPU under 8 GB)", got)
	}
}

func TestPromptInference_ExistingConfigBeatsHardware(t *testing.T) {
	// Existing config has Enabled=false. Hardware default would be
	// true. Interactive mode with empty input should preserve the
	// existing value.
	out := &bytes.Buffer{}
	in := strings.NewReader("\n") // accept first prompt default.
	got := promptInference(in, out,
		agentconfig.InferenceConfig{Enabled: false, ShareWithMesh: false}, true,
		gpuProfile(12288),
		nil, nil,
		false)
	if got.Enabled || got.ShareWithMesh {
		t.Errorf("got %+v, want Enabled=false (existing value preserved)", got)
	}
	if !strings.Contains(out.String(), "Existing agent.json found") {
		t.Errorf("expected re-enroll notice in output, got %q", out.String())
	}
}

func TestPromptInference_DisabledSkipsShareQuestion(t *testing.T) {
	out := &bytes.Buffer{}
	in := strings.NewReader("n\n") // answer N to Enabled.
	got := promptInference(in, out,
		agentconfig.InferenceConfig{}, false,
		gpuProfile(12288),
		nil, nil,
		false)
	if got.Enabled || got.ShareWithMesh {
		t.Errorf("got %+v, want Enabled=false ShareWithMesh=false", got)
	}
	if strings.Contains(out.String(), "Share this engine") {
		t.Errorf("share question should not appear when disabled, got %q", out.String())
	}
}

func TestPromptInference_InteractiveYY(t *testing.T) {
	out := &bytes.Buffer{}
	in := strings.NewReader("y\ny\n")
	got := promptInference(in, out,
		agentconfig.InferenceConfig{}, false,
		cpuProfile(), // hardware default would be N — operator overrides.
		nil, nil,
		false)
	if !got.Enabled || !got.ShareWithMesh {
		t.Errorf("got %+v, want Enabled=true ShareWithMesh=true (operator typed y/y)", got)
	}
}

func TestYnPrompt_UnparseableThenDefault(t *testing.T) {
	out := &bytes.Buffer{}
	sc := bufio.NewScanner(strings.NewReader("maybe\nperhaps\nwhat\n"))
	got := ynPrompt(out, sc, "Enable?", true)
	if !got {
		t.Errorf("got false, want default true after 3 bad answers")
	}
}

func TestYnPrompt_DefaultHintSpelledOut(t *testing.T) {
	// The hint must spell out the default ("default: Yes/No") so non-native
	// speakers aren't left to infer it from the [Y/n] capitalization alone.
	yes := &bytes.Buffer{}
	_ = ynPrompt(yes, bufio.NewScanner(strings.NewReader("\n")), "Enable?", true)
	if !strings.Contains(yes.String(), "default: Yes") {
		t.Errorf("default-true prompt missing 'default: Yes' hint, got %q", yes.String())
	}
	no := &bytes.Buffer{}
	_ = ynPrompt(no, bufio.NewScanner(strings.NewReader("\n")), "Enable?", false)
	if !strings.Contains(no.String(), "default: No") {
		t.Errorf("default-false prompt missing 'default: No' hint, got %q", no.String())
	}
}

func TestFlagBoolPtr_Unset(t *testing.T) {
	fs := newTestFlagSet()
	p := flagBoolPtr(fs, "x", "")
	if err := fs.Parse([]string{}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *p != nil {
		t.Errorf("want nil pointer when flag not passed, got %v", **p)
	}
}

func TestFlagBoolPtr_True(t *testing.T) {
	fs := newTestFlagSet()
	p := flagBoolPtr(fs, "x", "")
	if err := fs.Parse([]string{"--x"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *p == nil || **p != true {
		t.Errorf("want &true, got %v", *p)
	}
}

func TestFlagBoolPtr_FalseExplicit(t *testing.T) {
	fs := newTestFlagSet()
	p := flagBoolPtr(fs, "x", "")
	if err := fs.Parse([]string{"--x=false"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *p == nil || **p != false {
		t.Errorf("want &false, got %v", *p)
	}
}

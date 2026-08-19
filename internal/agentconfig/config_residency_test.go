package agentconfig

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Inference.IdleTimeout is the operator's model-residency setting
// (waired-agent#861). It was declared, defaulted, env-parsed and
// flag-registered long before it had a consumer, so these cover the four
// layers end to end rather than trusting that "the field exists".

func TestResidency_Default(t *testing.T) {
	// Product contract: owner ruling on waired-agent#861, recorded in
	// docs/decisions/20260820/0130-model-residency-is-a-setting.md.
	if got := Defaults().Inference.IdleTimeout.Duration(); got != 0 {
		t.Errorf("IdleTimeout default = %v, want 0 (hold indefinitely)", got)
	}
}

func TestResidency_JSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(path, []byte(`{"inference":{"idle_timeout":"30m"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatalf("MergeJSON: %v", err)
	}
	if got := cfg.Inference.IdleTimeout.Duration(); got != 30*time.Minute {
		t.Errorf("IdleTimeout from JSON = %v, want 30m", got)
	}
}

func TestResidency_Env(t *testing.T) {
	cfg := Defaults()
	if err := cfg.MergeEnv([]string{"WAIRED_INFERENCE_IDLE_TIMEOUT=45m"}); err != nil {
		t.Fatalf("MergeEnv: %v", err)
	}
	if got := cfg.Inference.IdleTimeout.Duration(); got != 45*time.Minute {
		t.Errorf("IdleTimeout from env = %v, want 45m", got)
	}
}

func TestResidency_Flag(t *testing.T) {
	cfg := Defaults()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg.RegisterInferenceFlags(fs)
	if err := fs.Parse([]string{"-inference-idle-timeout", "90s"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Inference.IdleTimeout.Duration(); got != 90*time.Second {
		t.Errorf("IdleTimeout from flag = %v, want 90s", got)
	}
}

// TestResidency_Precedence checks flag > env > JSON on this field
// specifically. The layers are hand-wired per field in this package, so
// a field can be half-wired and still compile.
func TestResidency_Precedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")
	if err := os.WriteFile(path, []byte(`{"inference":{"idle_timeout":"1m"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := Defaults()
	if err := cfg.MergeJSON(path); err != nil {
		t.Fatalf("MergeJSON: %v", err)
	}
	if got := cfg.Inference.IdleTimeout.Duration(); got != time.Minute {
		t.Fatalf("after JSON = %v, want 1m", got)
	}
	if err := cfg.MergeEnv([]string{"WAIRED_INFERENCE_IDLE_TIMEOUT=2m"}); err != nil {
		t.Fatalf("MergeEnv: %v", err)
	}
	if got := cfg.Inference.IdleTimeout.Duration(); got != 2*time.Minute {
		t.Fatalf("after env = %v, want 2m", got)
	}
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg.RegisterInferenceFlags(fs)
	if err := fs.Parse([]string{"-inference-idle-timeout", "3m"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.Inference.IdleTimeout.Duration(); got != 3*time.Minute {
		t.Errorf("after flag = %v, want 3m", got)
	}
}

// TestResidency_ZeroAndNegativeAreValid pins that the two spellings of
// "never unload on idle" survive Validate. A range check that rejected
// them would make the default unusable.
func TestResidency_ZeroAndNegativeAreValid(t *testing.T) {
	for _, d := range []time.Duration{0, -1, -time.Hour} {
		cfg := Defaults()
		cfg.Inference.IdleTimeout = Duration(d)
		if err := cfg.Validate(); err != nil {
			t.Errorf("Validate() with IdleTimeout=%v: %v", d, err)
		}
	}
}

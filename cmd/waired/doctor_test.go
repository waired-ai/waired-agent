package main

import (
	"os"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func TestFormatFinding_Icons(t *testing.T) {
	cases := []struct {
		in   integration.AuditFinding
		want string
	}{
		{integration.AuditFinding{Status: integration.StatusOK, Subject: "x", Detail: "y"}, "✓ x — y"},
		{integration.AuditFinding{Status: integration.StatusWarn, Subject: "x", Detail: "y"}, "⚠ x — y"},
		{integration.AuditFinding{Status: integration.StatusFail, Subject: "x", Detail: "y"}, "✗ x — y"},
		{integration.AuditFinding{Status: integration.StatusSkip, Subject: "x", Detail: "y"}, "· x — y"},
		{integration.AuditFinding{Status: integration.StatusOK, Subject: "no detail"}, "✓ no detail"},
	}
	for _, c := range cases {
		got := formatFinding(c.in)
		if got != c.want {
			t.Errorf("formatFinding(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCountFails_OnlyCountsStatusFail(t *testing.T) {
	findings := []integration.AuditFinding{
		{Status: integration.StatusOK},
		{Status: integration.StatusFail},
		{Status: integration.StatusFail},
		{Status: integration.StatusWarn},
		{Status: integration.StatusSkip},
	}
	if got := countFails(findings); got != 2 {
		t.Errorf("countFails = %d, want 2", got)
	}
}

func TestPhaseFinding_PausedEmitsWarn(t *testing.T) {
	dir := t.TempDir()
	w := state.NewWriter(dir, state.State{Phase: state.PhasePaused, GatewayURL: "http://127.0.0.1:9473", GatewayToken: "tok"})
	if err := w.Set(state.State{Phase: state.PhasePaused, GatewayURL: "http://127.0.0.1:9473", GatewayToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	got := phaseFinding(dir)
	if got.Status != integration.StatusWarn {
		t.Errorf("paused phase status = %s, want warn", got.Status)
	}
	if !strings.Contains(got.Detail, "waired resume") {
		t.Errorf("detail should suggest `waired resume`, got %q", got.Detail)
	}
}

func TestPhaseFinding_ActiveAndFreshEmitsOK(t *testing.T) {
	dir := t.TempDir()
	w := state.NewWriter(dir, state.State{Phase: state.PhaseActive, GatewayURL: "http://127.0.0.1:9473", GatewayToken: "tok"})
	if err := w.Set(state.State{Phase: state.PhaseActive, GatewayURL: "http://127.0.0.1:9473", GatewayToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	got := phaseFinding(dir)
	if got.Status != integration.StatusOK {
		t.Errorf("active+fresh status = %s, want ok", got.Status)
	}
}

func TestPhaseFinding_MissingStateIsSkipped(t *testing.T) {
	dir := t.TempDir()
	got := phaseFinding(dir)
	// Empty Subject signals "skip me" — the live probe handles
	// "daemon not running" with a better message.
	if got.Subject != "" {
		t.Errorf("missing state file should yield empty finding, got %+v", got)
	}
}

func TestPhaseFinding_StaleActiveIsSkipped(t *testing.T) {
	dir := t.TempDir()
	// Hand-craft a state file with an old `updated` timestamp so the
	// effective check fails. We bypass Writer because Writer always
	// stamps "now" — directly write the JSON.
	if err := os.MkdirAll(dir+"/runtime", 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{
  "phase": "active",
  "pid": 1,
  "updated": "2000-01-01T00:00:00Z",
  "gateway_url": "http://127.0.0.1:9473",
  "gateway_token": "tok"
}
`
	if err := os.WriteFile(dir+"/runtime/state", []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got := phaseFinding(dir)
	if got.Subject != "" {
		t.Errorf("stale active should yield empty finding, got %+v", got)
	}
}

func TestCollectDoctorFindings_HermeticMissingState(t *testing.T) {
	// Point at fresh tempdirs so the doctor reports "missing".
	home := t.TempDir()
	state := t.TempDir()
	findings := collectDoctorFindings(t.Context(), home, state, "http://127.0.0.1:65535", "http://127.0.0.1:65535")

	subjects := map[string]integration.Status{}
	for _, f := range findings {
		subjects[f.Subject] = f.Status
	}
	if got := subjects["gateway token"]; got != integration.StatusFail {
		t.Errorf("gateway token status = %s, want fail", got)
	}
	// Post-v2: env files and shell-rc snippets are no longer written
	// or audited. The doctor exposes the per-adapter audit + the live
	// probes as the "is it healthy?" surface.
	for _, f := range findings {
		if f.Subject == "env file" {
			t.Errorf("doctor should not report env file findings post-v2, got %+v", f)
		}
		if strings.HasPrefix(f.Subject, "shell-rc") {
			t.Errorf("doctor should not report shell-rc findings post-v2, got %+v", f)
		}
	}
	// Live probes will fail (no server on the wild port).
	probeFail := false
	for _, f := range findings {
		if strings.HasPrefix(f.Subject, "Local Gateway") && f.Status == integration.StatusFail {
			probeFail = true
		}
	}
	if !probeFail {
		t.Error("expected gateway probe to fail against unbound port")
	}
}

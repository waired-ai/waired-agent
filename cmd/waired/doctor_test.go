package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/platform/servicediag"
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

// TestDoctorHomeFor pins the fix for #650: under sudo the doctor inspects
// the invoking user's home, not root's. This is a product contract, not a
// record of today's behaviour — it is the defect the issue reports.
//
// Windows has no sudo hop (invokingSudoUserAt returns false for any goos
// that is not linux/darwin), and darwin is included because it only LOOKED
// correct before: macOS sudo keeps HOME, so its process home already was
// the user's. The seam has to prove that on purpose, not by accident.
func TestDoctorHomeFor(t *testing.T) {
	const (
		procHome = "/root"
		userHome = "/home/alice"
	)
	okLookup := func(u string) (string, error) {
		if u != "alice" {
			return "", fmt.Errorf("unexpected lookup of %q", u)
		}
		return userHome, nil
	}
	failLookup := func(string) (string, error) { return "", errors.New("nss miss") }
	emptyLookup := func(string) (string, error) { return "", nil }

	cases := []struct {
		name     string
		goos     string
		euid     int
		sudoUser string
		lookup   func(string) (string, error)
		want     doctorHome
	}{
		{
			name: "linux under sudo follows SUDO_USER", goos: "linux", euid: 0,
			sudoUser: "alice", lookup: okLookup,
			want: doctorHome{Dir: userHome, SudoUser: "alice"},
		},
		{
			name: "darwin under sudo follows SUDO_USER", goos: "darwin", euid: 0,
			sudoUser: "alice", lookup: okLookup,
			want: doctorHome{Dir: userHome, SudoUser: "alice"},
		},
		{
			name: "windows has no sudo hop", goos: "windows", euid: -1,
			sudoUser: "alice", lookup: okLookup,
			want: doctorHome{Dir: procHome},
		},
		{
			name: "unelevated uses the process home", goos: "linux", euid: 1000,
			sudoUser: "", lookup: okLookup,
			want: doctorHome{Dir: procHome},
		},
		{
			name: "a real root login is not a sudo hop", goos: "linux", euid: 0,
			sudoUser: "root", lookup: okLookup,
			want: doctorHome{Dir: procHome},
		},
		{
			name: "an unresolvable user falls back and says so", goos: "linux", euid: 0,
			sudoUser: "alice", lookup: failLookup,
			want: doctorHome{Dir: procHome, SudoUser: "alice", Fellback: true},
		},
		{
			name: "a user with no home falls back and says so", goos: "linux", euid: 0,
			sudoUser: "alice", lookup: emptyLookup,
			want: doctorHome{Dir: procHome, SudoUser: "alice", Fellback: true},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := doctorHomeFor(c.goos, c.euid, c.sudoUser, procHome, c.lookup)
			if got != c.want {
				t.Errorf("doctorHomeFor = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestDoctorHomeNotice(t *testing.T) {
	if n := (doctorHome{Dir: "/home/alice"}).notice(); n != "" {
		t.Errorf("an unelevated run should print no notice, got %q", n)
	}
	n := doctorHome{Dir: "/home/alice", SudoUser: "alice"}.notice()
	if !strings.Contains(n, `"alice"`) || !strings.Contains(n, "not root") {
		t.Errorf("sudo notice = %q, want it to name the user and say it is not root", n)
	}
	n = doctorHome{Dir: "/root", SudoUser: "alice", Fellback: true}.notice()
	if !strings.Contains(n, "/root") || !strings.Contains(n, "without sudo") {
		t.Errorf("fallback notice = %q, want the directory it actually used and the way out", n)
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
	// Zero trayDoctor: no tray finding, so the assertions below do not depend
	// on whether the test runner happens to have a desktop session.
	findings := collectDoctorFindings(t.Context(), home, state, "http://127.0.0.1:65535", "http://127.0.0.1:65535", trayDoctor{}, servicediag.Result{})

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

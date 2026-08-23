package main

import (
	"errors"
	"fmt"
	"io/fs"
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

// "1 findings need attention" was the literal closing line on a host with
// a single failure (#652). Four rendered correctly, so the defect only
// showed on the count an operator is most likely to see.
func TestFindingsSummary_Plural(t *testing.T) {
	cases := map[int]string{
		0: "0 findings need attention",
		1: "1 finding needs attention",
		2: "2 findings need attention",
	}
	for n, want := range cases {
		if got := findingsSummary(n); got != want {
			t.Errorf("findingsSummary(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestPhaseFinding_PausedEmitsWarn(t *testing.T) {
	dir := t.TempDir()
	w := state.NewWriter(dir, state.State{Phase: state.PhasePaused, GatewayURL: "http://127.0.0.1:9473"})
	if err := w.Set(state.State{Phase: state.PhasePaused, GatewayURL: "http://127.0.0.1:9473"}); err != nil {
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
	w := state.NewWriter(dir, state.State{Phase: state.PhaseActive, GatewayURL: "http://127.0.0.1:9473"})
	if err := w.Set(state.State{Phase: state.PhaseActive, GatewayURL: "http://127.0.0.1:9473"}); err != nil {
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
  "gateway_url": "http://127.0.0.1:9473"
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

// TestUnreadableFinding pins the permission-vs-absence split behind #651.
// The GOOS-varying half is elevationHint's, which has its own test; here
// the contract is only which errors produce a row at all.
func TestUnreadableFinding(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"permission denied is a check that did not run", fs.ErrPermission, true},
		{"wrapped permission denied still counts", fmt.Errorf("read x: %w", fs.ErrPermission), true},
		{"absent is not a skipped check", fs.ErrNotExist, false},
		{"a parse error is not a skipped check", errors.New("invalid character"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, ok := unreadableFinding("device sign-in", c.err)
			if ok != c.want {
				t.Fatalf("unreadableFinding(%v) ok = %v, want %v", c.err, ok, c.want)
			}
			if !ok {
				return
			}
			if f.Status != integration.StatusSkip {
				t.Errorf("status = %s, want skip", f.Status)
			}
			if f.Subject != "device sign-in" {
				t.Errorf("subject = %q, want the caller's subject verbatim", f.Subject)
			}
			if !strings.Contains(f.Detail, "needs elevation to check") {
				t.Errorf("detail = %q, want it to say the check did not run", f.Detail)
			}
		})
	}
}

// A skipped row must never move the exit code — the point of #651 is
// visibility, not a new failure mode.
func TestUnreadableFinding_DoesNotCountAsFailure(t *testing.T) {
	f, ok := unreadableFinding("waired phase", fs.ErrPermission)
	if !ok {
		t.Fatal("expected a finding")
	}
	if got := countFails([]integration.AuditFinding{f}); got != 0 {
		t.Errorf("countFails = %d, want 0", got)
	}
}

func TestCollectDoctorFindings_HermeticMissingState(t *testing.T) {
	// Point at fresh tempdirs so the doctor reports "missing".
	home := t.TempDir()
	state := t.TempDir()
	// Zero trayDoctor: no tray finding, so the assertions below do not depend
	// on whether the test runner happens to have a desktop session.
	findings := collectDoctorFindings(t.Context(), home, state, "http://127.0.0.1:65535", "http://127.0.0.1:65535", trayDoctor{}, servicediag.Result{}, claudeDoctor{})

	subjects := map[string]integration.Status{}
	for _, f := range findings {
		subjects[f.Subject] = f.Status
	}
	// The doctor used to carry a "gateway token" row here, failing when
	// <state>/secrets/gateway-token was absent and telling the user to run
	// `waired link` to create it. There is no such file and no such
	// credential any more (waired-ai/waired#1277), so the row must be gone
	// rather than reporting on something that cannot exist — a permanently
	// red row for a healthy host is worse than no row.
	if got, present := subjects["gateway token"]; present {
		t.Errorf("doctor still reports a %q row (status %s) — the credential is gone", "gateway token", got)
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

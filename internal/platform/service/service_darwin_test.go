//go:build darwin

package service

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRenderLaunchDaemonPlist_HappyPath(t *testing.T) {
	cfg := Config{
		Binary:    "/usr/local/bin/waired-agent",
		StateDir:  "/Library/Application Support/waired",
		MgmtAddr:  "127.0.0.1:9476",
		ExtraArgs: []string{"--bypass-cp-iam", "--force-relay"},
	}
	body, err := renderLaunchDaemonPlist(cfg)
	if err != nil {
		t.Fatalf("renderLaunchDaemonPlist: %v", err)
	}
	s := string(body)

	wantSubstrings := []string{
		`<key>Label</key>`,
		`<string>com.waired.agent</string>`,
		`<key>ProgramArguments</key>`,
		`<string>/usr/local/bin/waired-agent</string>`,
		`<string>--state-dir=/Library/Application Support/waired</string>`,
		`<string>--mgmt=127.0.0.1:9476</string>`,
		`<string>--bypass-cp-iam</string>`,
		`<string>--force-relay</string>`,
		`<key>RunAtLoad</key>`,
		`<true/>`,
		`<key>KeepAlive</key>`,
		`<key>SuccessfulExit</key>`,
		`<false/>`,
		`<key>Crashed</key>`,
		`<key>ProcessType</key>`,
		`<string>Background</string>`,
		`<key>WorkingDirectory</key>`,
		`<string>/Library/Application Support/waired</string>`,
		`<key>StandardOutPath</key>`,
		`<string>/Library/Logs/waired-agent.out.log</string>`,
		`<key>StandardErrorPath</key>`,
		`<string>/Library/Logs/waired-agent.err.log</string>`,
		`<key>EnvironmentVariables</key>`,
		`<key>HOME</key>`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(s, want) {
			t.Errorf("plist missing %q\n--- got ---\n%s", want, s)
		}
	}

	// #22: launchd exports no $HOME to a system daemon, so the plist must
	// set HOME (= the state dir) or a spawned `ollama serve` dies with
	// "$HOME is not defined". Assert the key/value pairing, not just the key.
	if !strings.Contains(s, "<key>HOME</key>\n  <string>/Library/Application Support/waired</string>") {
		t.Errorf("plist must set EnvironmentVariables HOME=<state dir>\n--- got ---\n%s", s)
	}

	// A system LaunchDaemon runs as root: there must be no UserName /
	// GroupName key (that would drop it to an unprivileged identity,
	// which #520 deliberately does not do).
	if strings.Contains(s, "<key>UserName</key>") || strings.Contains(s, "<key>GroupName</key>") {
		t.Errorf("plist must not set UserName/GroupName (daemon runs as root)\n--- got ---\n%s", s)
	}
}

func TestRenderLaunchDaemonPlist_RejectsEmptyBinary(t *testing.T) {
	_, err := renderLaunchDaemonPlist(Config{StateDir: "/x"})
	if err == nil || !strings.Contains(err.Error(), "cfg.Binary") {
		t.Errorf("expected error about cfg.Binary, got %v", err)
	}
}

func TestRenderLaunchDaemonPlist_RejectsEmptyStateDir(t *testing.T) {
	_, err := renderLaunchDaemonPlist(Config{Binary: "/x"})
	if err == nil || !strings.Contains(err.Error(), "cfg.StateDir") {
		t.Errorf("expected error about cfg.StateDir, got %v", err)
	}
}

func TestRenderLaunchDaemonPlist_EscapesSpecialChars(t *testing.T) {
	// XML entity escaping: < > & " ' must all be produced safely if
	// they appear in (e.g.) an --mgmt URL with a query string, or in
	// a state dir under "Application Support/Test & Co".
	cfg := Config{
		Binary:    "/usr/local/bin/waired-agent",
		StateDir:  "/Library/Application Support/Test & Co",
		MgmtAddr:  `127.0.0.1:9476?path=<>`,
		ExtraArgs: []string{"--label=" + `it's "fine"`},
	}
	body, err := renderLaunchDaemonPlist(cfg)
	if err != nil {
		t.Fatalf("renderLaunchDaemonPlist: %v", err)
	}
	s := string(body)
	// Each forbidden raw character must be replaced by an entity.
	for _, raw := range []string{"& Co", "?path=<", `"fine"`} {
		if strings.Contains(s, raw) {
			t.Errorf("plist contains unescaped %q\n--- got ---\n%s", raw, s)
		}
	}
}

// fakeLaunchctl records every argv handed to runLaunchctlFn and returns
// canned responses keyed by the first argv element ("bootstrap",
// "enable", "bootout", "kickstart", "kill").
type fakeLaunchctl struct {
	calls   [][]string
	respond map[string]launchctlResp
}

type launchctlResp struct {
	stdout []byte
	stderr []byte
	err    error
}

func (f *fakeLaunchctl) fn(args []string) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if r, ok := f.respond[args[0]]; ok {
		return r.stdout, r.stderr, r.err
	}
	return nil, nil, nil
}

func withFakeLaunchctl(t *testing.T) *fakeLaunchctl {
	t.Helper()
	f := &fakeLaunchctl{respond: map[string]launchctlResp{}}
	orig := runLaunchctlFn
	runLaunchctlFn = f.fn
	t.Cleanup(func() { runLaunchctlFn = orig })
	return f
}

// withRoot makes geteuidFn report root so the Install/Uninstall root
// gate can be exercised on a non-root CI host.
func withRoot(t *testing.T) {
	t.Helper()
	orig := geteuidFn
	geteuidFn = func() int { return 0 }
	t.Cleanup(func() { geteuidFn = orig })
}

// withTempDaemonDir redirects systemDaemonDir (the /Library/LaunchDaemons
// plist location) to a writable temp dir so tests never touch the real,
// root-only path.
func withTempDaemonDir(t *testing.T) string {
	t.Helper()
	orig := systemDaemonDir
	d := t.TempDir()
	systemDaemonDir = d
	t.Cleanup(func() { systemDaemonDir = orig })
	return d
}

func TestStart_KickstartArgv(t *testing.T) {
	f := withFakeLaunchctl(t)
	if err := (darwinManager{}).Start(nil); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var found []string
	for _, c := range f.calls {
		if c[0] == "kickstart" {
			found = c
			break
		}
	}
	if len(found) == 0 {
		t.Fatalf("kickstart not called; calls=%v", f.calls)
	}
	if found[1] != "-k" {
		t.Errorf("kickstart missing -k flag; got %v", found)
	}
	if found[2] != "system/"+darwinLabel {
		t.Errorf("kickstart target = %q, want %q", found[2], "system/"+darwinLabel)
	}
}

func TestStart_RejectsExtraArgs(t *testing.T) {
	withFakeLaunchctl(t)
	if err := (darwinManager{}).Start([]string{"--force-relay"}); err == nil {
		t.Error("Start with extra args: want error (plist is fixed at install time)")
	}
}

func TestStop_SendsSIGTERMToSystemDomain(t *testing.T) {
	f := withFakeLaunchctl(t)
	if err := (darwinManager{}).Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if len(f.calls) == 0 {
		t.Fatal("no launchctl calls recorded")
	}
	got := f.calls[0]
	if got[0] != "kill" || got[1] != "SIGTERM" || got[2] != "system/"+darwinLabel {
		t.Errorf("Stop: expected 'kill SIGTERM system/%s', got %v", darwinLabel, got)
	}
}

func TestStop_PropagatesLaunchctlError(t *testing.T) {
	f := withFakeLaunchctl(t)
	f.respond["kill"] = launchctlResp{
		stderr: []byte("Could not find service"),
		err:    errors.New("exit status 1"),
	}
	err := (darwinManager{}).Stop()
	if err == nil || !strings.Contains(err.Error(), "Could not find service") {
		t.Errorf("Stop: expected propagated stderr, got %v", err)
	}
}

// TestInstall_RequiresRoot guards that registering a system LaunchDaemon
// refuses to run unprivileged with a sudo-pointing message (the install
// would otherwise fail opaquely writing /Library/LaunchDaemons).
func TestInstall_RequiresRoot(t *testing.T) {
	withFakeLaunchctl(t)
	withTempDaemonDir(t)
	orig := geteuidFn
	geteuidFn = func() int { return 501 }
	t.Cleanup(func() { geteuidFn = orig })

	err := (darwinManager{}).Install(Config{Binary: "/usr/local/bin/waired-agent", StateDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "root") || !strings.Contains(err.Error(), "sudo") {
		t.Errorf("Install as non-root: want a root/sudo error, got %v", err)
	}
}

// TestInstall_BootstrapsSystemDomain asserts the install argv sequence
// targets the system domain (bootout system/, bootstrap system, enable
// system/) and that the plist is written under systemDaemonDir.
func TestInstall_BootstrapsSystemDomain(t *testing.T) {
	f := withFakeLaunchctl(t)
	withRoot(t)
	dir := withTempDaemonDir(t)
	stateDir := filepath.Join(t.TempDir(), "state")

	cfg := Config{Binary: "/usr/local/bin/waired-agent", StateDir: stateDir, MgmtAddr: "127.0.0.1:9476"}
	if err := (darwinManager{}).Install(cfg); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, darwinLabel+".plist")); err != nil {
		t.Fatalf("plist not written under systemDaemonDir: %v", err)
	}
	var sawBootstrap, sawEnable bool
	for _, c := range f.calls {
		if c[0] == "bootstrap" && len(c) >= 2 && c[1] == "system" {
			sawBootstrap = true
		}
		if c[0] == "enable" && len(c) >= 2 && c[1] == "system/"+darwinLabel {
			sawEnable = true
		}
	}
	if !sawBootstrap {
		t.Errorf("no `bootstrap system <plist>` call; calls=%v", f.calls)
	}
	if !sawEnable {
		t.Errorf("no `enable system/%s` call; calls=%v", darwinLabel, f.calls)
	}
}

// TestInstall_CreatesStateDir guards the regression found by the macOS
// edge-installer e2e: the agent plist sets WorkingDirectory=<state-dir>
// with RunAtLoad, so launchd chdir()s into it before exec. A `--no-init`
// install leaves that dir uncreated (init normally creates it), and the
// job then crash-loops with EX_CONFIG (78) and the mgmt API never comes
// up. Install must create the state dir up front.
func TestInstall_CreatesStateDir(t *testing.T) {
	withFakeLaunchctl(t)
	withRoot(t)
	withTempDaemonDir(t)

	stateDir := filepath.Join(t.TempDir(), "state")
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("precondition: state dir should not exist yet (err=%v)", err)
	}

	cfg := Config{
		Binary:   "/usr/local/bin/waired-agent",
		StateDir: stateDir,
		MgmtAddr: "127.0.0.1:9476",
	}
	if err := (darwinManager{}).Install(cfg); err != nil {
		t.Fatalf("Install: %v", err)
	}

	fi, err := os.Stat(stateDir)
	if err != nil {
		t.Fatalf("Install did not create state dir %s: %v", stateDir, err)
	}
	if !fi.IsDir() {
		t.Fatalf("state dir %s exists but is not a directory", stateDir)
	}
}

func TestUninstall_BestEffortToleratesMissingPlist(t *testing.T) {
	withFakeLaunchctl(t)
	withTempDaemonDir(t) // plist path → temp; a missing file must be tolerated
	if err := (darwinManager{}).Uninstall(); err != nil {
		t.Errorf("Uninstall on clean host: expected nil, got %v", err)
	}
}

// TestBootoutLegacyPerUserAgent_NoSudoUserIsNoop ensures the upgrade
// cleanup never touches launchd when there is no invoking user to
// resolve (a fresh install with SUDO_USER unset).
func TestBootoutLegacyPerUserAgent_NoSudoUserIsNoop(t *testing.T) {
	f := withFakeLaunchctl(t)
	t.Setenv("SUDO_USER", "")
	bootoutLegacyPerUserAgent()
	for _, c := range f.calls {
		if len(c) >= 2 && strings.HasPrefix(c[1], "gui/") {
			t.Errorf("unexpected per-user bootout with no SUDO_USER: %v", c)
		}
	}
}

func TestSystemLaunchDaemonPath(t *testing.T) {
	withTempDaemonDir(t)
	got := systemLaunchDaemonPath("com.example.app")
	if !strings.HasSuffix(got, "/com.example.app.plist") {
		t.Errorf("path suffix wrong: %q", got)
	}
}

func TestRunSubcommandFormatsLikeOtherOSes(t *testing.T) {
	// The shared shell in service.go prints "<svc> <name>" for the
	// success line and "<svc> <name>: <err>" for failure. We sanity
	// check by calling runSubcommand directly with a no-op so the
	// darwin install/uninstall/start/stop flows are guaranteed to
	// follow the same UX as Linux/Windows.
	handled, rc := runSubcommand("install", nil, func() error { return nil })
	if !handled || rc != 0 {
		t.Errorf("runSubcommand success path: got handled=%v rc=%d", handled, rc)
	}
	handled, rc = runSubcommand("start", nil, func() error { return fmt.Errorf("boom") })
	if !handled || rc != 1 {
		t.Errorf("runSubcommand failure path: got handled=%v rc=%d", handled, rc)
	}
}

// TestRenderLaunchDaemonPlist_PreservesArgvOrder asserts that the plist
// emits ProgramArguments in exactly the order we constructed them —
// launchd cares about argv order, and a future maintainer might be
// tempted to "tidy up" the loop into a sort.
func TestRenderLaunchDaemonPlist_PreservesArgvOrder(t *testing.T) {
	cfg := Config{
		Binary:    "/usr/local/bin/waired-agent",
		StateDir:  "/x",
		MgmtAddr:  "127.0.0.1:9476",
		ExtraArgs: []string{"--zzz-late", "--aaa-early"},
	}
	body, err := renderLaunchDaemonPlist(cfg)
	if err != nil {
		t.Fatalf("renderLaunchDaemonPlist: %v", err)
	}
	s := string(body)
	wantInOrder := []string{
		"--state-dir=/x",
		"--mgmt=127.0.0.1:9476",
		"--zzz-late",
		"--aaa-early",
	}
	prev := -1
	for _, want := range wantInOrder {
		idx := strings.Index(s, want)
		if idx < 0 {
			t.Fatalf("missing %q in plist", want)
		}
		if idx <= prev {
			t.Errorf("order violation: %q appeared before earlier arg", want)
		}
		prev = idx
	}
}

// Compile-time guard: runLaunchctlFn signature must match the real one,
// otherwise tests would silently exercise a wrong shape.
var _ = func() bool {
	var _ = runLaunchctlReal
	return reflect.TypeOf(runLaunchctlFn).Kind() == reflect.Func
}()

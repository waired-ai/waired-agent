package tray

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/appcontrol"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
	"github.com/waired-ai/waired-agent/internal/platform/trayhost"
)

// TestMain installs no-op stubs over every dialog / host-integration seam
// declared in tray.go, for the whole package, before any test runs.
//
// This is deliberately not a per-test opt-in. tray.go reaches showError from
// 48 places, and on darwin the real one opens a modal `osascript display
// dialog` that never returns without a click: one test that forgets to stub
// hangs the macOS runner to its job timeout instead of failing (#152 —
// TestRunPublicConsent_VersionMismatchRefetchesOnce was that test). Stubbing
// centrally means a NEW test cannot reintroduce the hang, which is the whole
// point of the change; scripts/ci/tray-dialog-seam-guard.sh keeps this file
// and the seam block in tray.go from drifting apart.
//
// The stubs record their arguments rather than discarding them, so what the
// user would have been shown is assertable (see seams). Answers default to
// the most conservative reading — "no dialog backend / user declined" — which
// is exactly what the real helpers return on a Linux CI box with no zenity,
// so turning the seams on changes no existing test's outcome.
func TestMain(m *testing.M) {
	installSeamStubs()
	// Seal the per-user state dir for the whole package. The autostart
	// first-run marker (waired-agent#1046) lives there, so without this
	// the suite reads and WRITES the developer's own
	// ~/.local/state/waired — and a marker left by one run silently
	// changes what the next run asserts. Machine-global state belongs in
	// TestMain, not in the tests that remember to ask (CLAUDE.md §Test
	// discipline); $WAIRED_STATE_DIR is what paths.StateDir honours
	// above everything else.
	dir, err := os.MkdirTemp("", "tray-state-*")
	if err != nil {
		panic("tray tests: cannot seal the state dir: " + err.Error())
	}
	os.Setenv(paths.EnvOverride, dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// seamLog records what the seamed helpers were asked to do. Handlers spawn
// goroutines (onLogout, startLogin), so the recording is mutex-guarded.
type seamLog struct {
	mu       sync.Mutex
	errors   []string
	confirms []string
	yesNos   []string
	// statuses records the bodies the status report dialog was asked to
	// show. statusCopy decides what that stubbed dialog answers, so a
	// test can drive both the "Close" and the "Copy details" arm.
	statuses   []string
	statusCopy bool
	abouts     int
	clipboard  []string
	browsers   []string
	elevations []string
	// links records the `waired link <target>` runs the Reconfigure rows
	// trigger. Not an elevation: the integration files belong to the
	// desktop user, so the CLI runs unelevated (waired-agent#986).
	links []string
	// Tray-host seams (#295). trayHostPlans records the Status each plan was
	// asked about, so a test can prove checkTrayHost fed the probe's verdict
	// through rather than deciding on its own.
	trayHostChecks  int
	trayHostPlans   []string
	trayHostEnables int
	// appControlChecks counts reads of the Windows Application Control log
	// (waired-agent#1217).
	appControlChecks int
}

func (l *seamLog) add(field *[]string, v string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*field = append(*field, v)
}

func (l *seamLog) snapshot(field *[]string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(*field))
	copy(out, *field)
	return out
}

// seams is the package-wide recording of seam traffic. Tests that care about
// it call resetSeams first; tests that do not simply ignore it.
var seams = &seamLog{}

// resetSeams clears the recording for one test and clears it again on
// cleanup, so a later test never reads a neighbour's dialogs.
func resetSeams(t *testing.T) *seamLog {
	t.Helper()
	// Field-by-field, not `*seams = seamLog{}`: that would copy the mutex
	// while holding it and unlock a different one on the way out.
	reset := func() {
		seams.mu.Lock()
		defer seams.mu.Unlock()
		seams.errors, seams.confirms, seams.yesNos = nil, nil, nil
		seams.statuses, seams.statusCopy = nil, false
		seams.clipboard, seams.browsers, seams.elevations = nil, nil, nil
		seams.links = nil
		seams.abouts = 0
		seams.trayHostChecks, seams.trayHostPlans, seams.trayHostEnables = 0, nil, 0
		seams.appControlChecks = 0
	}
	reset()
	t.Cleanup(reset)
	return seams
}

func installSeamStubs() {
	showAbout = func(string, string) {
		seams.mu.Lock()
		defer seams.mu.Unlock()
		seams.abouts++
	}
	showError = func(message string) { seams.add(&seams.errors, message) }
	showConfirm = func(prompt string) bool {
		seams.add(&seams.confirms, prompt)
		return false
	}
	confirmYesNo = func(title, body string) (bool, bool) {
		seams.add(&seams.yesNos, title+"\n"+body)
		return false, false
	}
	confirmWithLabels = func(_, _, _, _ string) (bool, bool) { return false, false }
	showStatus = func(body string) bool {
		seams.add(&seams.statuses, body)
		seams.mu.Lock()
		defer seams.mu.Unlock()
		return seams.statusCopy
	}
	copyToClipboard = func(text string) error {
		seams.add(&seams.clipboard, text)
		return nil
	}
	openBrowser = func(url string) error {
		seams.add(&seams.browsers, url)
		return nil
	}
	loginViaElevation = func(context.Context, string, string) error {
		seams.add(&seams.elevations, "login")
		return nil
	}
	logoutViaElevation = func(context.Context, string) error {
		seams.add(&seams.elevations, "logout")
		return nil
	}
	installOllamaViaElevation = func(context.Context, string) error {
		seams.add(&seams.elevations, "install-ollama")
		return nil
	}
	startAgentViaElevation = func(context.Context) error {
		seams.add(&seams.elevations, "start-agent")
		return nil
	}
	updateViaElevation = func(context.Context) error {
		seams.add(&seams.elevations, "update")
		return nil
	}
	linkIntegrationAsUser = func(_ context.Context, target string) error {
		seams.add(&seams.links, target)
		return nil
	}
	// Tray-host seams (#295). The real Check makes a D-Bus round trip against
	// the runner's session bus and the real Enable shells out to
	// gnome-extensions against the developer's OWN desktop — a unit test must
	// reach neither. The default reports a healthy host, which plans to
	// RepairNone, so checkTrayHost is a no-op for every test that does not
	// deliberately override these.
	trayHostCheck = func() trayhost.Result {
		seams.mu.Lock()
		defer seams.mu.Unlock()
		seams.trayHostChecks++
		return trayhost.Result{Status: trayhost.HostPresent, Desktop: trayhost.DesktopGNOME}
	}
	trayHostPlan = func(r trayhost.Result) trayhost.RepairAction {
		seams.mu.Lock()
		seams.trayHostPlans = append(seams.trayHostPlans, r.Status.String())
		seams.mu.Unlock()
		// The real planner is pure; only its fact-gathering touches the host,
		// so feed it facts that assert nothing about the runner.
		return trayhost.PlanRepair("linux", trayhost.RepairFacts{Status: r.Status, Desktop: r.Desktop})
	}
	trayHostEnable = func(context.Context) error {
		seams.mu.Lock()
		defer seams.mu.Unlock()
		seams.trayHostEnables++
		return nil
	}
	// Application Control seam (waired-agent#1217). The real Check shells out
	// to wevtutil against the runner's own CodeIntegrity log, which on a
	// Windows runner is a machine-global read no unit test should make. The
	// default reports nothing refused, so watchAppControl is a no-op for every
	// test that does not deliberately override this.
	appControlCheck = func(context.Context) appcontrol.Result {
		seams.mu.Lock()
		defer seams.mu.Unlock()
		seams.appControlChecks++
		return appcontrol.Result{Status: appcontrol.Clear}
	}
	// The real MenuLabels asks the session bus who owns
	// org.kde.StatusNotifierWatcher, so it too would read the developer's own
	// desktop. The default is the specification's dialect, which is also the
	// zero value a *tray built directly by a test carries
	// (waired-agent#1100).
	trayHostMenuLabels = func() trayhost.MenuDialect { return trayhost.MenuDialectSpec }
	// notifier is the pre-existing seam over the OS toast backend; on darwin
	// the real one also execs osascript. Give it the same package-wide
	// default so no test has to remember installStubNotifier just to stay
	// hermetic (tests that assert on toasts still install their own).
	notifier = &stubNotifier{}
}

// TestSeamStubsCoverEveryDeclaredSeam is the runtime half of
// scripts/ci/tray-dialog-seam-guard.sh: it proves that installSeamStubs
// actually replaced each seam, rather than leaving one pointing at the real
// per-OS helper. A seam added to tray.go without a stub here shows up as the
// hang this file exists to prevent, so it must fail loudly instead.
func TestSeamStubsCoverEveryDeclaredSeam(t *testing.T) {
	l := resetSeams(t)

	showAbout("v", "sha")
	showError("boom")
	showConfirm("really?")
	confirmYesNo("t", "b")
	showStatus("status body")
	copyToClipboard("clip")
	if err := openBrowser("https://example.invalid"); err != nil {
		t.Fatalf("openBrowser stub returned %v", err)
	}
	ctx := context.Background()
	for _, call := range []func() error{
		func() error { return loginViaElevation(ctx, "", "") },
		func() error { return logoutViaElevation(ctx, "") },
		func() error { return installOllamaViaElevation(ctx, "") },
		func() error { return updateViaElevation(ctx) },
		func() error { return startAgentViaElevation(ctx) },
	} {
		if err := call(); err != nil {
			t.Fatalf("elevation stub returned %v", err)
		}
	}
	if err := linkIntegrationAsUser(ctx, "opencode"); err != nil {
		t.Fatalf("linkIntegrationAsUser stub returned %v", err)
	}

	if l.abouts != 1 {
		t.Errorf("showAbout not stubbed: abouts = %d, want 1", l.abouts)
	}
	for name, got := range map[string][]string{
		"showError":       l.snapshot(&l.errors),
		"showConfirm":     l.snapshot(&l.confirms),
		"confirmYesNo":    l.snapshot(&l.yesNos),
		"showStatus":      l.snapshot(&l.statuses),
		"copyToClipboard": l.snapshot(&l.clipboard),
		"openBrowser":     l.snapshot(&l.browsers),
	} {
		if len(got) != 1 {
			t.Errorf("%s not stubbed: recorded %v, want exactly one call", name, got)
		}
	}
	if got := l.snapshot(&l.elevations); len(got) != 5 {
		t.Errorf("elevation seams not all stubbed: recorded %v, want 5 calls", got)
	}
	if got := l.snapshot(&l.links); len(got) != 1 {
		t.Errorf("linkIntegrationAsUser not stubbed: recorded %v, want exactly one call", got)
	}

	// confirmWithLabels predates #152 and has its own stub shape (labelStub);
	// assert only that the package default denies, which is what a host with
	// no dialog backend does.
	if yes, ok := confirmWithLabels("t", "b", "a", "c"); yes || ok {
		t.Errorf("confirmWithLabels default = (%v, %v), want (false, false)", yes, ok)
	}
}

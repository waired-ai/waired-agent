package tray

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newReconfigureTray builds a tray whose refresh poll (the goroutine every
// row action ends with) has somewhere to fail harmlessly: the handler under
// test is done by then, and a nil client would take the process down with
// it.
func newReconfigureTray(t *testing.T) *tray {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return &tray{cli: newTestClient(srv.URL)}
}

// writeOpenCodePlugin writes a waired.js registering provider.waired with
// the given baseURL, mirroring what the opencode adapter emits.
func writeOpenCodePlugin(t *testing.T, home, baseURL string) {
	t.Helper()
	dir := filepath.Join(home, ".config", "opencode", "plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `export const WairedPlugin = async () => ({
  config: async (config) => {
    config.provider = config.provider || {};
    config.provider.waired = { options: { baseURL: "` + baseURL + `" } };
  },
});
`
	if err := os.WriteFile(filepath.Join(dir, "waired.js"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeOpenClawPlugin writes an index.mjs registering the waired provider
// with the given BASE_URL, mirroring what the openclaw adapter emits.
func writeOpenClawPlugin(t *testing.T, home, baseURL string) {
	t.Helper()
	dir := filepath.Join(home, ".openclaw", "plugins", "waired")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `const BASE_URL = "` + baseURL + `";
export default { id: "waired", register(api) { api.registerProvider({ id: "waired" }); } };
`
	if err := os.WriteFile(filepath.Join(dir, "index.mjs"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestProbeReadsTheHomeItIsGiven is the regression guard for
// waired-agent#986: the row is a fact about ONE home, and the tray must
// read the home it runs in. The failing shape was a probe that ran in the
// daemon, where the answer came from the service account's home and every
// system-service install reported "not configured" while the plugin was
// right there in the desktop user's.
func TestProbeReadsTheHomeItIsGiven(t *testing.T) {
	const expected = "http://127.0.0.1:9473/v1"

	desktop := t.TempDir()
	writeOpenCodePlugin(t, desktop, expected)
	writeOpenClawPlugin(t, desktop, expected)
	service := t.TempDir() // the daemon's home: nothing was ever written here

	oc := probeOpenCode(desktop, expected)
	if oc == nil || !oc.Configured || oc.Stale {
		t.Errorf("opencode in the desktop home = %+v, want configured and fresh", oc)
	}
	if ow := probeOpenClaw(desktop, expected); ow == nil || !ow.Configured || ow.Stale {
		t.Errorf("openclaw in the desktop home = %+v, want configured and fresh", ow)
	}
	if got := probeOpenCode(service, expected); got == nil || got.Configured {
		t.Errorf("opencode in the service home = %+v, want not configured", got)
	}
	if got := probeOpenClaw(service, expected); got == nil || got.Configured {
		t.Errorf("openclaw in the service home = %+v, want not configured", got)
	}
}

func TestProbeReportsDriftAgainstTheDaemonsGatewayURL(t *testing.T) {
	home := t.TempDir()
	writeOpenCodePlugin(t, home, "http://127.0.0.1:9999/v1")
	writeOpenClawPlugin(t, home, "http://127.0.0.1:9999/v1")

	oc := probeOpenCode(home, "http://127.0.0.1:9473/v1")
	if oc == nil || !oc.Configured || !oc.Stale {
		t.Fatalf("opencode = %+v, want configured and stale", oc)
	}
	if oc.CurrentValue != "http://127.0.0.1:9999/v1" {
		t.Errorf("opencode CurrentValue = %q", oc.CurrentValue)
	}
	if ow := probeOpenClaw(home, "http://127.0.0.1:9473/v1"); ow == nil || !ow.Stale {
		t.Errorf("openclaw = %+v, want stale", ow)
	}
}

// TestProbeWithoutAHomeReportsNothing: an unresolvable home yields nil,
// which Update() renders as a hidden group. Saying "not configured"
// there would be the same lie in a different coat.
func TestProbeWithoutAHomeReportsNothing(t *testing.T) {
	if got := probeOpenCode("", "http://127.0.0.1:9473/v1"); got != nil {
		t.Errorf("probeOpenCode(\"\") = %+v, want nil", got)
	}
	if got := probeOpenClaw("", "http://127.0.0.1:9473/v1"); got != nil {
		t.Errorf("probeOpenClaw(\"\") = %+v, want nil", got)
	}
}

// TestTrayHomeMatchesTheAdapters: the probe and `waired link` must agree
// on which directory they are talking about. Both go through
// os.UserHomeDir() — $HOME on Unix, %USERPROFILE% on Windows — so a
// Windows session that exports HOME cannot split them.
func TestTrayHomeMatchesTheAdapters(t *testing.T) {
	want, err := os.UserHomeDir()
	if err != nil {
		// Not a skip: a host where this fails is one where the adapters
		// cannot resolve a home either, and the assertion is exactly what
		// would have caught that.
		t.Fatalf("os.UserHomeDir: %v", err)
	}
	if got := trayHome(); got != want {
		t.Errorf("trayHome() = %q, want os.UserHomeDir() = %q", got, want)
	}
}

// TestWairedLinkArgs pins the argv the Reconfigure rows run. --force
// because re-applying is the whole point of the row (an undetected agent
// must still be rewritten), --no-prompt because a menu click has no
// terminal to answer a question in, and NO --state-dir so the CLI resolves
// the same user-side ledger a person typing the command would get.
func TestWairedLinkArgs(t *testing.T) {
	got := wairedLinkArgs("opencode")
	want := []string{"link", "--force", "--no-prompt", "opencode"}
	if len(got) != len(want) {
		t.Fatalf("args = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args = %v, want %v", got, want)
		}
	}
}

// TestReconfigureRunsTheCLIAsThisUser covers both rows, and is the
// regression guard for the OpenCode row's missing click handler: it was
// created and shown but wired to nothing, so the item did nothing at all.
func TestReconfigureRunsTheCLIAsThisUser(t *testing.T) {
	for _, c := range []struct {
		name   string
		run    func(*tray, context.Context)
		target string
	}{
		{"opencode", (*tray).onReconfigureOpenCode, "opencode"},
		{"openclaw", (*tray).onReconfigureOpenClaw, "openclaw"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l := resetSeams(t)
			origConfirm := confirmYesNo
			confirmYesNo = func(string, string) (bool, bool) { return true, true }
			t.Cleanup(func() { confirmYesNo = origConfirm })

			tr := newReconfigureTray(t)
			c.run(tr, context.Background())

			if got := l.snapshot(&l.links); len(got) != 1 || got[0] != c.target {
				t.Fatalf("links = %v, want one %q run", got, c.target)
			}
			if got := l.snapshot(&l.elevations); len(got) != 0 {
				t.Errorf("elevations = %v, want none — the plugin belongs to this user", got)
			}
		})
	}
}

// TestReconfigureWithoutADialogOffersTheCommand: no dialog backend means
// no silent write. The clipboard gets the exact command the row's tooltip
// names, and nothing runs.
func TestReconfigureWithoutADialogOffersTheCommand(t *testing.T) {
	l := resetSeams(t)
	origConfirm := confirmYesNo
	confirmYesNo = func(string, string) (bool, bool) { return false, false }
	t.Cleanup(func() { confirmYesNo = origConfirm })

	newReconfigureTray(t).onReconfigureOpenCode(context.Background())

	if got := l.snapshot(&l.clipboard); len(got) != 1 || got[0] != "waired link opencode" {
		t.Errorf("clipboard = %v, want [waired link opencode]", got)
	}
	if got := l.snapshot(&l.links); len(got) != 0 {
		t.Errorf("links = %v, want none when there was no dialog to consent in", got)
	}
}

// TestReconfigureDeclinedRunsNothing: a dialog the user answered "no" to
// is not a fallback path — it is an answer.
func TestReconfigureDeclinedRunsNothing(t *testing.T) {
	l := resetSeams(t)
	origConfirm := confirmYesNo
	confirmYesNo = func(string, string) (bool, bool) { return false, true }
	t.Cleanup(func() { confirmYesNo = origConfirm })

	newReconfigureTray(t).onReconfigureOpenClaw(context.Background())

	if got := l.snapshot(&l.links); len(got) != 0 {
		t.Errorf("links = %v, want none", got)
	}
	if got := l.snapshot(&l.clipboard); len(got) != 0 {
		t.Errorf("clipboard = %v, want none — declining is not the no-dialog path", got)
	}
}

// TestReconfigureFailureSurfacesTheCLIWords: the notification carries the
// CLI's own last line, not "exit status 1".
func TestReconfigureFailureSurfacesTheCLIWords(t *testing.T) {
	l := resetSeams(t)
	origConfirm := confirmYesNo
	confirmYesNo = func(string, string) (bool, bool) { return true, true }
	origLink := linkIntegrationAsUser
	linkIntegrationAsUser = func(context.Context, string) error {
		return errors.New("exit status 1: integration: opencode: cannot resolve $HOME")
	}
	t.Cleanup(func() {
		confirmYesNo = origConfirm
		linkIntegrationAsUser = origLink
	})

	newReconfigureTray(t).onReconfigureOpenCode(context.Background())

	errs := l.snapshot(&l.errors)
	if len(errs) != 1 {
		t.Fatalf("errors = %v, want one", errs)
	}
	if want := "cannot resolve $HOME"; !strings.Contains(errs[0], want) {
		t.Errorf("error = %q, want it to carry %q", errs[0], want)
	}
}

// TestLastMeaningfulLine keeps the CLI-output reader honest about blank
// trailing lines and about clamping.
func TestLastMeaningfulLine(t *testing.T) {
	if got := lastMeaningfulLine("first\nlast line\n\n"); got != "last line" {
		t.Errorf("got %q", got)
	}
	if got := lastMeaningfulLine("   \n\n"); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	long := strings.Repeat("x", 300)
	if got := lastMeaningfulLine(long); len(got) != 200 {
		t.Errorf("len = %d, want the 200-char clamp", len(got))
	}
}

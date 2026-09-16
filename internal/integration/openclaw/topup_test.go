package openclaw

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/modelrows"
)

// swapWindow replaces the gateway seam for one test.
func swapWindow(t *testing.T, n int) {
	t.Helper()
	prev := contextWindowFn
	contextWindowFn = func(context.Context, string, string) int { return n }
	t.Cleanup(func() { contextWindowFn = prev })
}

// PRODUCT CONTRACT (waired-agent#1029): a plugin written before this host
// could say what it serves is filled in once it can.
//
// The window is written ONCE, by the CLI, at link time — and `waired init`
// applies the integrations before anything is serving, so on a fresh install
// the gateway cannot be asked and the plugin correctly declares nothing
// (docs/decisions/20260822/2116: "a wrong number is worse than none").
// Nothing ever went back, so the host stayed on OpenClaw's own default
// forever. Observed on a vLLM host whose gateway had been answering 124928
// for as long as the engine had been up.
func TestTopUpContextWindow_FillsInAWindowThePluginNeverKnew(t *testing.T) {
	opts := newOpts(t)
	swapWindow(t, 0) // the wizard's moment: nothing is serving yet
	a := New()
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if declared, ok := DeclaredContextWindow(opts.HomeDir); !ok || declared != 0 {
		t.Fatalf("declared=%d ok=%v, want the undeclared plugin this fixture is about", declared, ok)
	}

	swapWindow(t, 124928) // the engine is up now
	got, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL)
	if err != nil || !changed || got != 124928 {
		t.Fatalf("TopUpContextWindow = (%d, %v, %v), want (124928, true, nil)", got, changed, err)
	}
	body, err := os.ReadFile(PluginEntryFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "const CONTEXT_WINDOW = 124928;") {
		t.Errorf("the plugin still does not declare the window:\n%s", body)
	}
	// The rest of the plugin is untouched — this rewrites one number, not
	// the integration.
	if !strings.Contains(string(body), providerBaseURL(opts.GatewayBaseURL)) {
		t.Error("the top-up lost the provider baseURL")
	}
}

// The three cases that must do nothing, each for its own reason. A top-up
// that fired on any of them would be worse than the gap it closes.
func TestTopUpContextWindow_LeavesItAlone(t *testing.T) {
	t.Run("no plugin installed", func(t *testing.T) {
		opts := newOpts(t)
		swapWindow(t, 200704)
		got, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL)
		if err != nil || changed || got != 0 {
			t.Fatalf("= (%d, %v, %v), want (0, false, nil) — nothing to top up", got, changed, err)
		}
	})

	t.Run("the gateway cannot answer", func(t *testing.T) {
		opts := newOpts(t)
		swapWindow(t, 200704)
		if err := New().Apply(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		// The daemon is down now: a gateway that cannot answer must never
		// BLANK a declaration that was right.
		swapWindow(t, 0)
		got, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL)
		if err != nil || changed || got != 200704 {
			t.Fatalf("= (%d, %v, %v), want (200704, false, nil)", got, changed, err)
		}
		body, _ := os.ReadFile(PluginEntryFile(opts.HomeDir))
		if !strings.Contains(string(body), "const CONTEXT_WINDOW = 200704;") {
			t.Errorf("an unanswerable gateway rewrote the plugin:\n%s", body)
		}
	})

	t.Run("already correct", func(t *testing.T) {
		opts := newOpts(t)
		swapWindow(t, 200704)
		if err := New().Apply(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(PluginEntryFile(opts.HomeDir))
		if err != nil {
			t.Fatal(err)
		}
		got, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL)
		if err != nil || changed || got != 200704 {
			t.Fatalf("= (%d, %v, %v), want (200704, false, nil)", got, changed, err)
		}
		after, _ := os.ReadFile(PluginEntryFile(opts.HomeDir))
		if string(before) != string(after) {
			t.Error("a no-op top-up rewrote the file")
		}
	})

	t.Run("a hand-edited plugin without the line", func(t *testing.T) {
		opts := newOpts(t)
		swapWindow(t, 200704)
		if err := New().Apply(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(PluginEntryFile(opts.HomeDir),
			[]byte("// somebody's own plugin\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		swapWindow(t, 124928)
		if _, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL); err != nil || changed {
			t.Fatalf("changed=%v err=%v — a plugin waired did not write is not ours to correct", changed, err)
		}
	})
}

// A row's window is compared, not only its key: a computer that switched to a
// model with a different window keeps its row, and the plugin used to keep the
// old number until the next link (waired-agent#1395).
func TestTopUpContextWindow_RewritesARowWhoseWindowAloneChanged(t *testing.T) {
	opts := newOpts(t)
	swapWindow(t, 200704)
	row := func(win int) []modelrows.Row {
		return []modelrows.Row{
			{DirectiveModel: claudecode.DirectiveModel{ID: "waired/default", DisplayName: "Waired"}, ContextWindow: 200704},
			{DirectiveModel: claudecode.DirectiveModel{ID: "waired/peer-linux-gpu", DisplayName: "Waired peer: linux-gpu"}, ContextWindow: win},
		}
	}
	swapRowsFn(t, row(262144))
	if err := New().Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := auditContextWindow(context.Background(), opts); got.Status != integration.StatusOK {
		t.Fatalf("a freshly written plugin audits as %v: %s", got.Status, got.Detail)
	}

	swapRowsFn(t, row(1048576))
	if got := auditContextWindow(context.Background(), opts); got.Status != integration.StatusWarn {
		t.Errorf("a row whose window changed audits as %v: %s", got.Status, got.Detail)
	}
	_, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v, want the plugin rewritten", changed, err)
	}
	body, _ := os.ReadFile(PluginEntryFile(opts.HomeDir))
	if !strings.Contains(string(body), `"key":"peer-linux-gpu","name":"Waired peer: linux-gpu","contextWindow":1048576`) {
		t.Errorf("the row still carries its old window:\n%s", body)
	}
}

// A plugin an older template wrote is rewritten even when every row still
// matches: what changed is what it sends (waired-agent#1395 — the any-computer
// row is sent as "waired").
func TestTopUpContextWindow_RewritesAnOlderRevision(t *testing.T) {
	opts := newOpts(t)
	swapWindow(t, 200704)
	if err := New().Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(PluginEntryFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	// What a revision-1 plugin looks like: no PLUGIN_REV line. Replaced
	// without its line ending, which is CRLF in a Windows build's embed.
	old := strings.Replace(string(body), "const PLUGIN_REV = 2;", "", 1)
	if old == string(body) {
		t.Fatal("fixture: the revision line was not found")
	}
	if err := os.WriteFile(PluginEntryFile(opts.HomeDir), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := auditContextWindow(context.Background(), opts); got.Status != integration.StatusWarn {
		t.Errorf("an older plugin audits as %v: %s", got.Status, got.Detail)
	}
	_, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v, want the older plugin rewritten", changed, err)
	}
	if got := declaredRevision(opts.HomeDir); got != pluginRevision {
		t.Errorf("revision after the top-up = %d, want %d", got, pluginRevision)
	}
}

// The drift `waired doctor` reports, and the three ways it must stay quiet.
func TestAuditContextWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		written    int // -1 = do not install a plugin
		live       int
		wantStatus integration.Status
		wantSubstr string
	}{
		{"declared and current", 200704, 200704, integration.StatusOK, "200704 tokens"},
		{"declared nothing, the host now serves a window", 0, 124928, integration.StatusWarn, "declares no window"},
		{"declared a stale window", 200704, 124928, integration.StatusWarn, "declares 200704"},
		{"the gateway cannot be asked", 200704, 0, integration.StatusOK, "did not report a window"},
		{"no plugin at all", -1, 200704, integration.StatusOK, "no plugin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := newOpts(t)
			if tc.written >= 0 {
				swapWindow(t, tc.written)
				if err := New().Apply(context.Background(), opts); err != nil {
					t.Fatal(err)
				}
			}
			swapWindow(t, tc.live)
			got := auditContextWindow(context.Background(), opts)
			if got.Status != tc.wantStatus {
				t.Errorf("status = %v, want %v (detail: %s)", got.Status, tc.wantStatus, got.Detail)
			}
			if !strings.Contains(got.Detail, tc.wantSubstr) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tc.wantSubstr)
			}
			// PRODUCT CONTRACT (waired-agent#1298): the subject is the
			// exported constant. `waired doctor` matches on it to decide
			// that this warning is FIXABLE — a warning does not earn the
			// repair prompt otherwise — so a rename here would silently
			// take the fix away rather than break a build.
			if got.Subject != ContextWindowSubject {
				t.Errorf("subject = %q, want %q", got.Subject, ContextWindowSubject)
			}
		})
	}
}

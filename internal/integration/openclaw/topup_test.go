package openclaw

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/modelrows"
)

// writeOlderPlugin installs what a build before waired-agent#1396 left: the
// current template with the window it was told and the revision before this
// one. Built from the current template rather than a hand-written file, so the
// lines the readers match are the real ones.
func writeOlderPlugin(t *testing.T, opts integration.ApplyOptions, window int) {
	t.Helper()
	if err := New().Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(PluginEntryFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(string(body), "const CONTEXT_WINDOW = 200704;", "const CONTEXT_WINDOW = "+strconv.Itoa(window)+";", 1)
	old = strings.Replace(old, "const PLUGIN_REV = "+strconv.Itoa(pluginRevision)+";", "const PLUGIN_REV = "+strconv.Itoa(pluginRevision-1)+";", 1)
	if old == string(body) {
		t.Fatal("fixture: the window and revision lines were not found")
	}
	if err := os.WriteFile(PluginEntryFile(opts.HomeDir), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
}

// PRODUCT CONTRACT (waired-agent#1029, #1396): a plugin an older build wrote is
// brought to what this build writes — 200704 for every row without "[1m]" —
// without a re-link. Two windows such a build declared: none, when it linked
// before anything served, and the window the gateway stated at the time, which
// on a vLLM host was the engine's own (124928 measured on 0.0.3-rc6).
func TestTopUpContextWindow_RewritesAPluginAnOlderBuildWrote(t *testing.T) {
	for _, old := range []int{0, 124928, 262144} {
		t.Run(strconv.Itoa(old), func(t *testing.T) {
			swapRowsFn(t, nil)
			opts := newOpts(t)
			writeOlderPlugin(t, opts, old)

			got, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL)
			if err != nil || !changed || got != 200704 {
				t.Fatalf("TopUpContextWindow = (%d, %v, %v), want (200704, true, nil)", got, changed, err)
			}
			body, err := os.ReadFile(PluginEntryFile(opts.HomeDir))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(body), "const CONTEXT_WINDOW = 200704;") {
				t.Errorf("the plugin still declares its old window:\n%s", body)
			}
			if got := declaredRevision(opts.HomeDir); got != pluginRevision {
				t.Errorf("revision after the top-up = %d, want %d", got, pluginRevision)
			}
			// The rest of the plugin is untouched.
			if !strings.Contains(string(body), providerBaseURL(opts.GatewayBaseURL)) {
				t.Error("the top-up lost the provider baseURL")
			}
		})
	}
}

// A plugin of the current revision that declares another window — edited by
// hand, since this build never writes one — is rewritten too: the revision
// alone is not what says the window is right.
func TestTopUpContextWindow_RewritesAnotherWindowAtTheCurrentRevision(t *testing.T) {
	swapRowsFn(t, nil)
	opts := newOpts(t)
	if err := New().Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(PluginEntryFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(body), "const CONTEXT_WINDOW = 200704;", "const CONTEXT_WINDOW = 131072;", 1)
	if err := os.WriteFile(PluginEntryFile(opts.HomeDir), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	got, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL)
	if err != nil || !changed || got != 200704 {
		t.Fatalf("TopUpContextWindow = (%d, %v, %v), want (200704, true, nil)", got, changed, err)
	}
	if declared, _ := DeclaredContextWindow(opts.HomeDir); declared != 200704 {
		t.Errorf("declared = %d after the top-up, want 200704", declared)
	}
}

// The cases that must do nothing, each for its own reason.
func TestTopUpContextWindow_LeavesItAlone(t *testing.T) {
	t.Run("no plugin installed", func(t *testing.T) {
		swapRowsFn(t, nil)
		opts := newOpts(t)
		got, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL)
		if err != nil || changed || got != 0 {
			t.Fatalf("= (%d, %v, %v), want (0, false, nil) — nothing to top up", got, changed, err)
		}
	})

	t.Run("already current, the gateway not answering", func(t *testing.T) {
		// The daemon is down: a list nobody could read is not a reason to
		// rewrite a plugin that is right.
		swapRowsFn(t, nil)
		opts := newOpts(t)
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
		swapRowsFn(t, nil)
		opts := newOpts(t)
		if err := New().Apply(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(PluginEntryFile(opts.HomeDir),
			[]byte("// somebody's own plugin\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL); err != nil || changed {
			t.Fatalf("changed=%v err=%v — a plugin waired did not write is not ours to correct", changed, err)
		}
	})
}

// A row carries no window, so a computer that switched to a model with a
// different window changes nothing in the plugin: its row is still a 200k
// session (waired-agent#1396). A row that appears does.
func TestTopUpContextWindow_RowsAreComparedByKeyAndName(t *testing.T) {
	opts := newOpts(t)
	rows := func(win int, extra ...modelrows.Row) []modelrows.Row {
		return append([]modelrows.Row{
			{DirectiveModel: claudecode.DirectiveModel{ID: "waired/default", DisplayName: "Waired"}, ContextWindow: 200704},
			{DirectiveModel: claudecode.DirectiveModel{ID: "waired/peer-linux-gpu", DisplayName: "Waired peer: linux-gpu"}, ContextWindow: win},
		}, extra...)
	}
	swapRowsFn(t, rows(200704))
	if err := New().Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := auditContextWindow(context.Background(), opts); got.Status != integration.StatusOK {
		t.Fatalf("a freshly written plugin audits as %v: %s", got.Status, got.Detail)
	}

	swapRowsFn(t, rows(1048576))
	if got := auditContextWindow(context.Background(), opts); got.Status != integration.StatusOK {
		t.Errorf("a row whose stated window alone changed audits as %v: %s", got.Status, got.Detail)
	}
	if _, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL); err != nil || changed {
		t.Fatalf("changed=%v err=%v, want the plugin left as it is", changed, err)
	}

	swapRowsFn(t, rows(200704, modelrows.Row{
		DirectiveModel: claudecode.DirectiveModel{ID: "waired/default[1m]", DisplayName: "Waired (1M context)"},
		ContextWindow:  1048576,
	}))
	if got := auditContextWindow(context.Background(), opts); got.Status != integration.StatusWarn {
		t.Errorf("a new row audits as %v: %s", got.Status, got.Detail)
	}
	if _, changed, err := TopUpContextWindow(context.Background(), opts.HomeDir, opts.GatewayBaseURL); err != nil || !changed {
		t.Fatalf("changed=%v err=%v, want the plugin rewritten", changed, err)
	}
	body, _ := os.ReadFile(PluginEntryFile(opts.HomeDir))
	if !strings.Contains(string(body), `{"key":"default[1m]","name":"Waired (1M context)"}`) {
		t.Errorf("the new row did not reach the plugin:\n%s", body)
	}
}

// The drift `waired doctor` reports, and the ways it must stay quiet.
func TestAuditContextWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		older      int // -1 = current plugin, -2 = no plugin
		sameRev    bool
		wantStatus integration.Status
		wantSubstr string
	}{
		{"current", -1, false, integration.StatusOK, "200704 tokens"},
		{"no plugin at all", -2, false, integration.StatusOK, "no plugin"},
		{"written by an older version", 124928, false, integration.StatusWarn, "older version of Waired"},
		// The two window findings, reached with the revision line current: a
		// plugin edited by hand, since this build never writes either.
		{"declares no window", 0, true, integration.StatusWarn, "declares no window for waired/default; Waired now gives it 200704 tokens"},
		{"declares another window", 124928, true, integration.StatusWarn, "declares 124928 tokens for waired/default; Waired now gives it 200704"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			swapRowsFn(t, nil)
			opts := newOpts(t)
			switch {
			case tc.older == -1:
				if err := New().Apply(context.Background(), opts); err != nil {
					t.Fatal(err)
				}
			case tc.older >= 0:
				writeOlderPlugin(t, opts, tc.older)
				if tc.sameRev {
					body, _ := os.ReadFile(PluginEntryFile(opts.HomeDir))
					cur := strings.Replace(string(body), "const PLUGIN_REV = "+strconv.Itoa(pluginRevision-1)+";",
						"const PLUGIN_REV = "+strconv.Itoa(pluginRevision)+";", 1)
					if err := os.WriteFile(PluginEntryFile(opts.HomeDir), []byte(cur), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
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

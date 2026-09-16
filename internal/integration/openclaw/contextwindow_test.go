package openclaw

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Every Waired row is one of two sessions, decided by its id: 1048576 tokens
// for a "[1m]" row, 200704 for every other, whichever computer answers it. The
// plugin works that out from the key, so a key it was never given — a computer
// that joined after the last link — gets the same window as the rows it was.
//
// PRODUCT CONTRACT, ratifying source: owner decision 2026-09-16 on
// waired-agent#1396. It inverts #1001's contract (the window the gateway
// stated for waired/default when the plugin was written) and waired-agent#1395's
// (each row's own stated window, and none for a row that stated none).
func TestRenderEntry_EveryRowsWindowFollowsItsKey(t *testing.T) {
	body, err := renderEntry("http://127.0.0.1:9473", pluginRows(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"const CONTEXT_WINDOW = 200704;",
		"const CONTEXT_WINDOW_1M = 1048576;",
		`return key.toLowerCase().includes("[1m]") ? CONTEXT_WINDOW_1M : CONTEXT_WINDOW;`,
		"contextWindow: windowFor(key),",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered plugin missing %q:\n%s", want, s)
		}
	}
	// Nothing reads a window off a row or leaves the field out any more.
	for _, gone := range []string{"row.contextWindow", "if (window > 0)", "32768"} {
		if strings.Contains(s, gone) {
			t.Errorf("rendered plugin still carries %q:\n%s", gone, s)
		}
	}
}

// A link before anything serves — `waired init` applies the integrations
// first — writes the same windows as any other. It used to declare none, and
// OpenClaw then sized every session by its own default until something went
// back to the plugin (#1029).
func TestApply_DeclaresTheWindowWithNothingServing(t *testing.T) {
	swapRowsFn(t, nil)
	opts := newOpts(t)
	if err := New().Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	entry, err := os.ReadFile(PluginEntryFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(entry), "const CONTEXT_WINDOW = 200704;") {
		t.Errorf("plugin does not declare the 200k window:\n%s", entry)
	}
	if got, ok := DeclaredContextWindow(opts.HomeDir); !ok || got != 200704 {
		t.Errorf("DeclaredContextWindow = %d, %v, want 200704", got, ok)
	}
	if _, ok := loadRec(t, opts); !ok {
		t.Error("no ledger record written")
	}
}

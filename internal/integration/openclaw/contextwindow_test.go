package openclaw

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/modelrows"
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
//
// One exception since waired-ai/waired#1481 (from owner rulings 3 and 5 on
// waired-ai/waired#1473): a row carrying a window below 200704 — a computer
// serving a custom model that small — takes it. Such a row carries it, and no
// other row carries a window at all (pluginRows).
func TestRenderEntry_EveryRowsWindowFollowsItsKey(t *testing.T) {
	body, err := renderEntry("http://127.0.0.1:9473", pluginRows(nil))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	for _, want := range []string{
		"const CONTEXT_WINDOW = 200704;",
		"const CONTEXT_WINDOW_1M = 1048576;",
		`if (key.toLowerCase().includes("[1m]")) return CONTEXT_WINDOW_1M;`,
		`if (row && typeof row.window === "number" && row.window > 0 && row.window < CONTEXT_WINDOW) return row.window;`,
		"contextWindow: windowFor(key),",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered plugin missing %q:\n%s", want, s)
		}
	}
	// Nothing leaves the field out, or reads the pre-#1396 field name.
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

// pluginRows carries a window only on a row whose listing states one below
// 200704 (waired-ai/waired#1481); every other row is written as before, so a
// plugin for a fleet with no small custom model is byte-for-byte unchanged.
func TestPluginRows_CarryOnlyASmallCustomWindow(t *testing.T) {
	rows := pluginRows([]modelrows.Row{
		{DirectiveModel: claudecode.DirectiveModel{ID: "waired/local", DisplayName: "Waired local"}, ContextWindow: 40960},
		{DirectiveModel: claudecode.DirectiveModel{ID: "waired/peer", DisplayName: "Waired peer"}, ContextWindow: 200704},
		{DirectiveModel: claudecode.DirectiveModel{ID: "waired/peer-box", DisplayName: "Waired peer: box"}, ContextWindow: 1048576},
	})
	want := []pluginRow{{Key: "local", Name: "Waired local", OwnWindow: 40960}, {Key: "peer", Name: "Waired peer"}, {Key: "peer-box", Name: "Waired peer: box"}}
	if !sameRows(rows, want) {
		t.Errorf("rows %+v, want %+v", rows, want)
	}
	body, err := renderEntry("http://127.0.0.1:9473", rows)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `{"key":"local","name":"Waired local","window":40960}`) ||
		!strings.Contains(string(body), `{"key":"peer","name":"Waired peer"}`) {
		t.Errorf("rendered rows:\n%s", body)
	}
}

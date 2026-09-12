package openclaw

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/modelrows"
)

func row(id, name string, window int) modelrows.Row {
	return modelrows.Row{
		DirectiveModel: claudecode.DirectiveModel{ID: id, DisplayName: name},
		ContextWindow:  window,
	}
}

// swapRowsFn replaces the gateway read for one test. The fetch itself is
// exercised directly against an httptest server in internal/integration/
// modelrows, so replacing it here leaves no implementation untested
// (CLAUDE.md §Test discipline).
func swapRowsFn(t *testing.T, rows []modelrows.Row) *string {
	t.Helper()
	var askedBase string
	prev := rowsFn
	rowsFn = func(_ context.Context, base string) []modelrows.Row {
		askedBase = base
		return rows
	}
	t.Cleanup(func() { rowsFn = prev })
	return &askedBase
}

// PIN: product contract — the rows say which COMPUTER answers, and the owner
// asked for the same set OpenCode and Claude Code offer (rc6 review,
// waired-ai/waired#1349, waired-agent#1306). Where they have to land is not a
// choice: OpenClaw's picker shows what agents.defaults.models allowlists, so a
// row that reaches the plugin and not that file is invisible. Measured on
// OpenClaw 2026.9.4 (2026-09-12).
func TestApply_WritesTheGatewaysRowsToBothPlaces(t *testing.T) {
	base := swapRowsFn(t, []modelrows.Row{
		row("waired/default", "Waired", 0),
		row("waired/local", "Waired local", 131072),
		row("waired/peer", "Waired peer", 0),
		row("waired/peer-linux-gpu", "Waired peer: linux-gpu", 200704),
	})
	opts := newOpts(t)
	if err := New().Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if *base != "http://127.0.0.1:9473" {
		t.Errorf("asked %q for the rows, want the gateway base URL", *base)
	}

	body, err := os.ReadFile(PluginEntryFile(opts.HomeDir))
	if err != nil {
		t.Fatalf("read plugin entry: %v", err)
	}
	s := string(body)
	// The rows are keyed by the id minus its "waired/" head, because that is
	// what OpenClaw hands resolveDynamicModel.
	if !strings.Contains(s, `{"key":"peer-linux-gpu","name":"Waired peer: linux-gpu","contextWindow":200704}`) {
		t.Errorf("the peer row did not reach the plugin with its own window:\n%s", s)
	}
	if !strings.Contains(s, `{"key":"local","name":"Waired local","contextWindow":131072}`) {
		t.Errorf("the local row did not reach the plugin:\n%s", s)
	}

	models := navModels(readJSON(t, ConfigFile(opts.HomeDir)))
	for _, ref := range []string{"waired/default", "waired/local", "waired/peer", "waired/peer-linux-gpu"} {
		if _, ok := models[ref]; !ok {
			t.Errorf("agents.defaults.models missing %q: %v", ref, models)
		}
	}
}

// A row names a computer, and computers leave the mesh. A re-link must take
// the departed one's row away: leaving it would be a menu entry whose
// selection fails, which is the thing the picker cannot render as disabled.
func TestApply_RelinkPrunesARowForAComputerThatLeft(t *testing.T) {
	opts := newOpts(t)
	swapRowsFn(t, []modelrows.Row{row("waired/default", "Waired", 0), row("waired/peer-linux-gpu", "Waired peer: linux-gpu", 0)})
	if err := New().Apply(context.Background(), opts); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if _, ok := navModels(readJSON(t, ConfigFile(opts.HomeDir)))["waired/peer-linux-gpu"]; !ok {
		t.Fatal("the first link did not allowlist the peer row")
	}

	swapRowsFn(t, []modelrows.Row{row("waired/default", "Waired", 0)})
	if err := New().Apply(context.Background(), opts); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	models := navModels(readJSON(t, ConfigFile(opts.HomeDir)))
	if _, ok := models["waired/peer-linux-gpu"]; ok {
		t.Errorf("a row for a computer that left survived the re-link: %v", models)
	}
	if _, ok := models["waired/default"]; !ok {
		t.Errorf("the re-link took the surviving row too: %v", models)
	}
}

// An empty answer is "not known", not "no rows": the agent may not be running
// yet — `waired init` configures coding tools before anything is serving — and
// the one row that needs no facts about a mesh is what this integration
// shipped before waired-agent#1306.
func TestApply_NoAnswerFallsBackToTheOneRowThatNeedsNoMesh(t *testing.T) {
	swapRowsFn(t, nil)
	opts := newOpts(t)
	if err := New().Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	models := navModels(readJSON(t, ConfigFile(opts.HomeDir)))
	if len(models) != 1 {
		t.Errorf("models = %v, want only the default row", models)
	}
	if _, ok := models["waired/default"]; !ok {
		t.Errorf("models = %v, want waired/default", models)
	}
}

// The bare any-node spelling cannot be addressed here: OpenClaw reads a model
// reference as <provider>/<model>, so a key has to be the second segment. An
// allowlist entry of "waired" is read as the model "waired" on the provider
// "openai" (measured on 2026.9.4).
func TestPluginRows_SkipsAnIdWithNoSecondSegment(t *testing.T) {
	rows := pluginRows([]modelrows.Row{row("waired", "Waired", 0), row("waired/peer", "Waired peer", 0)})
	if len(rows) != 1 || rows[0].Key != "peer" {
		t.Errorf("rows = %+v, want only the addressable one", rows)
	}
}

// Uninstall clears the whole head rather than a list, so it also clears rows
// written by a build that knew names this one does not.
func TestUninstall_ClearsEveryRowUnderTheHead(t *testing.T) {
	swapRowsFn(t, []modelrows.Row{row("waired/default", "Waired", 0), row("waired/peer-linux-gpu", "Waired peer: linux-gpu", 0)})
	opts := newOpts(t)
	// A model of the user's own, so the file survives the uninstall and the
	// assertion is about what was removed rather than about the file going.
	if err := os.MkdirAll(ConfigDir(opts.HomeDir), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := []byte(`{"agents":{"defaults":{"models":{"ollama/qwen3.5-9b":{}}}}}`)
	if err := os.WriteFile(ConfigFile(opts.HomeDir), seed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := New().Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := New().Uninstall(context.Background(), opts); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	cfg := readJSON(t, ConfigFile(opts.HomeDir))
	for _, ref := range []string{"waired/default", "waired/peer-linux-gpu"} {
		if _, ok := navModels(cfg)[ref]; ok {
			t.Errorf("%q survived uninstall: %v", ref, cfg)
		}
	}
	if _, ok := navModels(cfg)["ollama/qwen3.5-9b"]; !ok {
		t.Errorf("uninstall took a model the user owns: %v", cfg)
	}
}

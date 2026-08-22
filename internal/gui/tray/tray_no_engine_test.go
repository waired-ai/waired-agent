package tray

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// Product contract (waired-agent#852, owner ruling 2026-08-19): a model
// row on a host with NO AI engine stays clickable — #842 removed the
// ability to grey a model out on any surface, and waired#1067's
// warn-and-ask ruling is about capacity, not about whether an engine is
// here — and the click says there is no engine and offers to install
// one. Recording the choice alone would change nothing a person could
// see.

// labelRecorder is labelStub plus the button labels, because the default
// button is part of what is being asserted: the offer must not be armed
// on the affirmative.
type labelRecorder struct {
	confirmed, ok  bool
	title, body    string
	affirm, negate string
	calls          int
}

func (s *labelRecorder) install(t *testing.T) {
	t.Helper()
	orig := confirmWithLabels
	confirmWithLabels = func(title, body, affirm, negate string) (bool, bool) {
		s.calls++
		s.title, s.body, s.affirm, s.negate = title, body, affirm, negate
		return s.confirmed, s.ok
	}
	t.Cleanup(func() { confirmWithLabels = orig })
}

// installRecorder counts the elevated engine installs and can fail them.
type installRecorder struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (r *installRecorder) install(t *testing.T) {
	t.Helper()
	orig := installOllamaViaElevation
	installOllamaViaElevation = func(context.Context, string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls++
		return r.err
	}
	t.Cleanup(func() { installOllamaViaElevation = orig })
}

func (r *installRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func trayWithNoEngine(cli *Client, entries ...CatalogEntryView) *tray {
	tr := trayWithEntries(cli, entries...)
	tr.last.CatalogEngineMissing = true
	return tr
}

func someRow() CatalogEntryView {
	return CatalogEntryView{ModelID: "qwen3-8b-instruct", Name: "Qwen3 8B Instruct"}
}

func TestOnSelectCatalogEntry_NoEngineOffersToInstallOne(t *testing.T) {
	f := &preferredModelFake{}
	dlg := &labelRecorder{confirmed: true, ok: true}
	dlg.install(t)
	inst := &installRecorder{}
	inst.install(t)

	tr := trayWithNoEngine(f.start(t), someRow())
	tr.onSelectCatalogEntry(context.Background(), 0)

	if dlg.calls != 1 {
		t.Fatalf("dialogs asked = %d, want exactly one", dlg.calls)
	}
	if dlg.affirm != "Install" || dlg.negate != "Not now" {
		t.Errorf("buttons = %q / %q, want Install / Not now", dlg.affirm, dlg.negate)
	}
	// Both halves of the truth. "No engine" alone reads as "this
	// computer is broken", and it is not: an engine-less host stays
	// enrolled and its requests go to the mesh (#387, #841).
	if !strings.Contains(dlg.body, "no inference engine installed here") {
		t.Errorf("the question must say the engine is absent: %q", dlg.body)
	}
	if !strings.Contains(dlg.body, "go to your other computers") {
		t.Errorf("the question must say where the requests go: %q", dlg.body)
	}
	if !strings.Contains(dlg.body, "Qwen3 8B Instruct") {
		t.Errorf("the question must name the model: %q", dlg.body)
	}
	if inst.count() != 1 {
		t.Errorf("engine installs = %d, want 1", inst.count())
	}
	if got := f.snapshot(); len(got) != 1 || got[0] != "qwen3-8b-instruct" {
		t.Errorf("posts = %v, want the choice recorded after the install", got)
	}
}

func TestOnSelectCatalogEntry_NoEngineDeclinedChangesNothing(t *testing.T) {
	f := &preferredModelFake{}
	dlg := &labelRecorder{confirmed: false, ok: true}
	dlg.install(t)
	inst := &installRecorder{}
	inst.install(t)

	tr := trayWithNoEngine(f.start(t), someRow())
	tr.onSelectCatalogEntry(context.Background(), 0)

	if dlg.calls != 1 {
		t.Fatalf("dialogs asked = %d, want exactly one", dlg.calls)
	}
	if inst.count() != 0 {
		t.Errorf("engine installs = %d after Not now, want 0", inst.count())
	}
	if got := f.snapshot(); len(got) != 0 {
		t.Errorf("posts = %v, want nothing recorded after Not now", got)
	}
}

// A failed install must not leave a preference behind. The order is the
// point: writing it first would post a switch to a daemon with nothing
// to switch, which on Windows reaches the restart fallback and the
// service that does not come back (#855).
func TestOnSelectCatalogEntry_NoEngineFailedInstallRecordsNothing(t *testing.T) {
	f := &preferredModelFake{}
	dlg := &labelRecorder{confirmed: true, ok: true}
	dlg.install(t)
	inst := &installRecorder{err: errors.New("elevation cancelled")}
	inst.install(t)

	tr := trayWithNoEngine(f.start(t), someRow())
	tr.onSelectCatalogEntry(context.Background(), 0)

	if inst.count() != 1 {
		t.Fatalf("engine installs = %d, want 1 attempt", inst.count())
	}
	if got := f.snapshot(); len(got) != 0 {
		t.Errorf("posts = %v, want nothing recorded when the install failed", got)
	}
}

// No dialog backend is not consent. Hand over the terminal equivalent,
// the same way an unaskable unfit switch does.
func TestOnSelectCatalogEntry_NoEngineWithoutADialogHandsOverTheCommand(t *testing.T) {
	f := &preferredModelFake{}
	dlg := &labelRecorder{confirmed: true, ok: false}
	dlg.install(t)
	inst := &installRecorder{}
	inst.install(t)

	var copied []string
	origCopy := copyToClipboard
	copyToClipboard = func(text string) error { copied = append(copied, text); return nil }
	t.Cleanup(func() { copyToClipboard = origCopy })

	tr := trayWithNoEngine(f.start(t), someRow())
	tr.onSelectCatalogEntry(context.Background(), 0)

	if inst.count() != 0 {
		t.Errorf("engine installs = %d, want 0 — no dialog is not a yes", inst.count())
	}
	if len(f.snapshot()) != 0 {
		t.Errorf("posts = %v, want none", f.snapshot())
	}
	if len(copied) != 1 || !strings.Contains(copied[0], "waired runtimes install ollama") {
		t.Errorf("clipboard = %v, want the install command", copied)
	}
}

// The engine question is about the HOST, so it is asked before the
// per-row one and instead of it — otherwise a row that also fails the
// capacity check would ask twice about two different things.
func TestOnSelectCatalogEntry_NoEngineAsksBeforeTheUnfitQuestion(t *testing.T) {
	f := &preferredModelFake{}
	dlg := &labelRecorder{confirmed: false, ok: true}
	dlg.install(t)
	inst := &installRecorder{}
	inst.install(t)

	row := someRow()
	row.UnfitReason = "needs 24 GB VRAM (have 8 GB)"
	row.UnfitKind = UnfitMemory
	tr := trayWithNoEngine(f.start(t), row)
	tr.onSelectCatalogEntry(context.Background(), 0)

	if dlg.calls != 1 {
		t.Fatalf("dialogs asked = %d, want exactly one", dlg.calls)
	}
	if !strings.Contains(dlg.title, "no inference engine") {
		t.Errorf("title = %q, want the engine question", dlg.title)
	}
	if strings.Contains(dlg.body, "24 GB VRAM") {
		t.Errorf("the memory shortfall leaked into the engine question: %q", dlg.body)
	}
}

// A host WITH an engine keeps the pre-#852 behaviour exactly.
func TestOnSelectCatalogEntry_EngineInstalledSwitchesWithoutAsking(t *testing.T) {
	f := &preferredModelFake{}
	dlg := &labelRecorder{confirmed: true, ok: true}
	dlg.install(t)
	inst := &installRecorder{}
	inst.install(t)

	tr := trayWithEntries(f.start(t), someRow())
	tr.onSelectCatalogEntry(context.Background(), 0)

	if dlg.calls != 0 {
		t.Errorf("dialogs asked = %d on a fitting row with an engine, want 0", dlg.calls)
	}
	if inst.count() != 0 {
		t.Errorf("engine installs = %d, want 0", inst.count())
	}
	if got := f.snapshot(); len(got) != 1 {
		t.Errorf("posts = %v, want the ordinary switch", got)
	}
}

// TestApplyCatalog_NoEngineNoteKeepsEveryRow pins the projection half:
// the context line appears, and the rows below it are untouched. The
// verdicts stay because they are true about what this computer would run
// once an engine is installed, and the catalog is also a browse surface.
func TestApplyCatalog_NoEngineNoteKeepsEveryRow(t *testing.T) {
	families := []management.CatalogFamily{
		{ModelID: "qwen3-8b-instruct", DisplayName: "Qwen3 8B Instruct", Fits: true},
		{ModelID: "qwen3-4b-instruct", DisplayName: "Qwen3 4B Instruct", Fits: true},
	}
	no, yes := false, true

	project := func(installed *bool) MenuModel {
		var m MenuModel
		applyCatalog(&m, &management.ModelCatalogResponse{
			Engine: "ollama", Families: families, EngineInstalled: installed,
		})
		return m
	}

	base := project(nil)

	missing := project(&no)
	if !missing.CatalogEngineMissing {
		t.Error("CatalogEngineMissing = false on a host with no engine")
	}
	for _, want := range []string{"No inference engine on this computer", "your other computers"} {
		if !strings.Contains(missing.CatalogNoteLabel, want) {
			t.Errorf("note %q is missing %q", missing.CatalogNoteLabel, want)
		}
	}
	if len(missing.CatalogEntries) != len(base.CatalogEntries) {
		t.Fatalf("rows = %d, want %d — the note must not withdraw the catalog",
			len(missing.CatalogEntries), len(base.CatalogEntries))
	}
	for i := range missing.CatalogEntries {
		if missing.CatalogEntries[i] != base.CatalogEntries[i] {
			t.Errorf("row %d changed: %+v vs %+v", i, missing.CatalogEntries[i], base.CatalogEntries[i])
		}
	}

	// nil is unknown, not absent, and an installed engine says nothing:
	// an older daemon renders exactly the pre-#852 submenu.
	for name, m := range map[string]MenuModel{"nil": base, "installed": project(&yes)} {
		if m.CatalogNoteLabel != "" || m.CatalogEngineMissing {
			t.Errorf("%s: note=%q missing=%v, want silence", name, m.CatalogNoteLabel, m.CatalogEngineMissing)
		}
	}
}

// TestEngineInstallNoDialogTextQuotesOnlyTheCommand is the same defect on
// the tray's fallback path (#852). The clipboard hand-off matters most:
// what is copied has to run when pasted, and it used to carry
// "(from an elevated prompt)" inside it.
func TestEngineInstallNoDialogTextQuotesOnlyTheCommand(t *testing.T) {
	for goos, want := range map[string]string{
		"linux":   `Cannot ask here — run "sudo waired runtimes install ollama" in a terminal to install the inference engine.`,
		"darwin":  `Cannot ask here — run "sudo waired runtimes install ollama" in a terminal to install the inference engine.`,
		"windows": `Cannot ask here — run "waired runtimes install ollama" from an elevated prompt to install the inference engine.`,
	} {
		if got := engineInstallNoDialogText(goos); got != want {
			t.Errorf("%s: %q, want %q", goos, got, want)
		}
	}
}

func TestOnSelectCatalogEntry_NoEngineCopiesAPasteableCommand(t *testing.T) {
	f := &preferredModelFake{}
	(&labelRecorder{confirmed: true, ok: false}).install(t)
	(&installRecorder{}).install(t)

	var copied []string
	orig := copyToClipboard
	copyToClipboard = func(text string) error { copied = append(copied, text); return nil }
	t.Cleanup(func() { copyToClipboard = orig })

	tr := trayWithNoEngine(f.start(t), someRow())
	tr.onSelectCatalogEntry(context.Background(), 0)

	if len(copied) != 1 {
		t.Fatalf("clipboard writes = %d, want 1", len(copied))
	}
	for _, banned := range []string{"(", ")", "elevated", "prompt"} {
		if strings.Contains(copied[0], banned) {
			t.Errorf("clipboard %q carries prose (%q); it must run when pasted", copied[0], banned)
		}
	}
}

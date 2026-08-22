package tray

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/platform/notification"
)

// Product contract (waired-agent#831, the 2026-08-08 warn-and-ask ruling
// recorded as waired-ai/waired#1067): clicking a model this computer is
// not expected to run asks, with the shortfall and a default of No. It
// used to be a silent no-op behind a greyed row — the tray was the last
// surface still refusing, which is why "switching the model from the
// tray does nothing" while the same switch through the setup wizard
// worked.

// preferredModelFake records the switches the tray posts.
type preferredModelFake struct {
	mu    sync.Mutex
	posts []string
}

func (f *preferredModelFake) start(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/inference/preferred-model" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var req management.PreferredModelRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.posts = append(f.posts, req.ModelID)
		f.mu.Unlock()
		writeJSONResp(w, http.StatusAccepted, management.PreferredModelResponse{ModelID: req.ModelID})
	}))
	t.Cleanup(srv.Close)
	return newTestClient(srv.URL)
}

func (f *preferredModelFake) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.posts))
	copy(out, f.posts)
	return out
}

func trayWithEntries(cli *Client, entries ...CatalogEntryView) *tray {
	return &tray{cli: cli, lastCatalogEntries: entries}
}

func TestOnSelectCatalogEntry_UnfitRowAsksThenSwitches(t *testing.T) {
	f := &preferredModelFake{}
	stub := &labelStub{confirmed: true, ok: true}
	stub.install(t)
	tr := trayWithEntries(f.start(t), CatalogEntryView{
		ModelID:     "qwen3-32b-instruct",
		Name:        "Qwen3 32B Instruct",
		UnfitReason: "needs 24 GB VRAM (have 8 GB)",
		UnfitKind:   UnfitMemory,
	})

	tr.onSelectCatalogEntry(context.Background(), 0)

	if stub.calls != 1 {
		t.Fatalf("dialogs asked = %d, want exactly one", stub.calls)
	}
	if !strings.Contains(stub.gotBody, "needs 24 GB VRAM (have 8 GB)") {
		t.Errorf("the question must carry the shortfall: %q", stub.gotBody)
	}
	if !strings.Contains(stub.gotBody, "Qwen3 32B Instruct") {
		t.Errorf("the question must name the model: %q", stub.gotBody)
	}
	if got := f.snapshot(); len(got) != 1 || got[0] != "qwen3-32b-instruct" {
		t.Errorf("posts = %v, want the confirmed switch to go through", got)
	}
}

func TestOnSelectCatalogEntry_UnfitRowDeclinedDoesNotSwitch(t *testing.T) {
	f := &preferredModelFake{}
	stub := &labelStub{confirmed: false, ok: true}
	stub.install(t)
	tr := trayWithEntries(f.start(t), CatalogEntryView{
		ModelID:     "qwen3-32b-instruct",
		Name:        "Qwen3 32B Instruct",
		UnfitReason: "needs 24 GB VRAM (have 8 GB)",
		UnfitKind:   UnfitMemory,
	})

	tr.onSelectCatalogEntry(context.Background(), 0)

	if stub.calls != 1 {
		t.Fatalf("dialogs asked = %d, want exactly one", stub.calls)
	}
	if got := f.snapshot(); len(got) != 0 {
		t.Errorf("posts = %v, want none — declining is an answer", got)
	}
}

// No zenity, no kdialog, no osascript: the click cannot be answered, so
// it does not switch — but it does not go quiet either. That silence is
// the second half of #831, and the way out is the CLI, which runs this
// same gate on stdin.
func TestOnSelectCatalogEntry_NoDialogBackendSaysSoAndDoesNotSwitch(t *testing.T) {
	f := &preferredModelFake{}
	log := resetSeams(t)
	toasts := installStubNotifier(t)
	(&labelStub{confirmed: false, ok: false}).install(t)
	tr := trayWithEntries(f.start(t), CatalogEntryView{
		ModelID:     "qwen3-32b-instruct",
		Name:        "Qwen3 32B Instruct",
		UnfitReason: "needs 24 GB VRAM (have 8 GB)",
		UnfitKind:   UnfitMemory,
	})

	tr.onSelectCatalogEntry(context.Background(), 0)

	if got := f.snapshot(); len(got) != 0 {
		t.Errorf("posts = %v, want none — an unanswerable question is not consent", got)
	}
	calls := toasts.snapshot()
	if len(calls) != 1 {
		t.Fatalf("toasts = %v, want exactly one — the click must not be silent", calls)
	}
	if !strings.Contains(calls[0].body, "waired models use qwen3-32b-instruct") {
		t.Errorf("the toast must name the command that can answer it: %q", calls[0].body)
	}
	if calls[0].level != notification.Warning {
		t.Errorf("toast level = %v, want Warning", calls[0].level)
	}
	if got := log.snapshot(&log.clipboard); len(got) != 1 || got[0] != "waired models use qwen3-32b-instruct" {
		t.Errorf("clipboard = %v, want the command ready to paste", got)
	}
}

func TestOnSelectCatalogEntry_FittingRowSwitchesWithoutAsking(t *testing.T) {
	f := &preferredModelFake{}
	stub := &labelStub{confirmed: true, ok: true}
	stub.install(t)
	tr := trayWithEntries(f.start(t), CatalogEntryView{
		ModelID: "qwen3-4b-instruct",
		Name:    "Qwen3 4B",
	})

	tr.onSelectCatalogEntry(context.Background(), 0)

	if stub.calls != 0 {
		t.Errorf("dialogs asked = %d, want none for a model that runs here", stub.calls)
	}
	if got := f.snapshot(); len(got) != 1 || got[0] != "qwen3-4b-instruct" {
		t.Errorf("posts = %v, want the switch to go straight through", got)
	}
}

// A slot past the end of the projection draws no row, so there is no
// click to answer: it stays silent, and must not ask either.
func TestOnSelectCatalogEntry_EmptySlotIsSilent(t *testing.T) {
	f := &preferredModelFake{}
	stub := &labelStub{confirmed: true, ok: true}
	stub.install(t)
	tr := trayWithEntries(f.start(t))

	tr.onSelectCatalogEntry(context.Background(), 3)

	if stub.calls != 0 || len(f.snapshot()) != 0 {
		t.Errorf("an empty slot did something: dialogs=%d posts=%v", stub.calls, f.snapshot())
	}
}

func TestUnfitSwitchPrompt_ShortfallReadsAsASentence(t *testing.T) {
	title, body := unfitSwitchPrompt("Qwen3 32B Instruct", UnfitMemory, "needs 24 GB VRAM (have 8 GB)")
	if title != "This model does not fit this computer" {
		t.Errorf("title: %q", title)
	}
	want := "Qwen3 32B Instruct does not fit in this computer's memory: " +
		"needs 24 GB VRAM (have 8 GB).\n\n" +
		"Loading it is expected to fail. Switch to it anyway?"
	if body != want {
		t.Errorf("body:\n got %q\nwant %q", body, want)
	}
}

// PRODUCT CONTRACT (waired-agent#850): a verdict this layer cannot
// explain is repeated, not explained. The engine-version floor leaves no
// hostfit reason to read (internal/router/family_picker.go), and the
// first version of this dialog put its text into the memory sentence —
// "does not fit in this computer's memory: needs ollama ≥ 0.32.13", on a
// host with 63 GB free.
func TestUnfitSwitchPrompt_UnpricedVerdictIsEchoedNotExplained(t *testing.T) {
	title, body := unfitSwitchPrompt(
		"Qwen3.8 27B", UnfitOther, "needs ollama ≥ 0.32.13 (running unknown version)")
	if title != "This model does not run on this computer" {
		t.Errorf("title: %q", title)
	}
	want := "Qwen3.8 27B — needs ollama ≥ 0.32.13 (running unknown version)\n\n" +
		"Selecting it is expected to fail. Switch to it anyway?"
	if body != want {
		t.Errorf("body:\n got %q\nwant %q", body, want)
	}
	if strings.Contains(body, "memory") {
		t.Errorf("the dialog named a cause the verdict does not carry: %q", body)
	}
}

// The other verdict is not a quantity, so it does not get forced after a
// colon — it repeats the row's own words, which name the engine the
// catalog has no variant for (waired-ai/waired#1272; inverted from the
// pre-#1272 "never name a variant or an engine" pin).
func TestUnfitSwitchPrompt_NoBuildHereGetsItsOwnWords(t *testing.T) {
	title, body := unfitSwitchPrompt("DeepSeek V4 Flash", UnfitNoBuild, "no Ollama variant")
	if title != "This model does not run on this computer" {
		t.Errorf("title: %q", title)
	}
	want := "DeepSeek V4 Flash has no Ollama variant.\n\n" +
		"Selecting it is expected to fail. Switch to it anyway?"
	if body != want {
		t.Errorf("body:\n got %q\nwant %q", body, want)
	}
}

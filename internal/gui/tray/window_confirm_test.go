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
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// waired-ai/waired#1456 (owner requirement 2026-09-20: warn, then serve;
// copy owner-approved 2026-09-20): the 1M preset asks first, default No,
// with the console's words. The 200K preset does not ask.

// windowPostFake records the (model, window) pairs the tray posts.
type windowPostFake struct {
	mu    sync.Mutex
	posts []management.PreferredModelRequest
}

func (f *windowPostFake) start(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/inference/preferred-model" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var req management.PreferredModelRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.posts = append(f.posts, req)
		f.mu.Unlock()
		writeJSONResp(w, http.StatusAccepted, management.PreferredModelResponse{ModelID: req.ModelID})
	}))
	t.Cleanup(srv.Close)
	return newTestClient(srv.URL)
}

func (f *windowPostFake) snapshot() []management.PreferredModelRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]management.PreferredModelRequest(nil), f.posts...)
}

// trayAtPresets is a tray whose window presets were last drawn for
// "reaches", with 200K in force.
func trayAtPresets(cli *Client) *tray {
	return &tray{
		cli:             cli,
		lastWindowModel: "reaches",
		lastWindowRows: []WindowRow{
			{Label: "200K", Tokens: hostfit.ServingWindow200k, Wall: WindowReachable, Selected: true},
			{Label: "1M", Tokens: hostfit.ServingWindow1M, Wall: WindowReachable},
		},
	}
}

func TestOnSelectWindow_1MAsksFirstAndKeep200KChangesNothing(t *testing.T) {
	f := &windowPostFake{}
	stub := &labelStub{confirmed: false, ok: true}
	stub.install(t)
	tr := trayAtPresets(f.start(t))

	tr.onSelectWindow(context.Background(), 1)

	if stub.calls != 1 {
		t.Fatalf("dialogs asked = %d, want exactly one", stub.calls)
	}
	title, body := longWindowPrompt()
	if stub.gotTitle != title || stub.gotBody != body {
		t.Errorf("asked %q / %q, want the approved words", stub.gotTitle, stub.gotBody)
	}
	if got := f.snapshot(); len(got) != 0 {
		t.Errorf("posts = %+v, want none — Keep 200K leaves everything as it is", got)
	}
}

func TestOnSelectWindow_Use1MAnywayServesAt1M(t *testing.T) {
	f := &windowPostFake{}
	installStubNotifier(t)
	(&labelStub{confirmed: true, ok: true}).install(t)
	tr := trayAtPresets(f.start(t))

	tr.onSelectWindow(context.Background(), 1)

	got := f.snapshot()
	if len(got) != 1 || got[0].ModelID != "reaches" || got[0].ContextWindow != hostfit.ServingWindow1M {
		t.Errorf("posts = %+v, want the model in use at 1M", got)
	}
}

func TestOnSelectWindow_200KDoesNotAsk(t *testing.T) {
	f := &windowPostFake{}
	installStubNotifier(t)
	stub := &labelStub{confirmed: false, ok: true}
	stub.install(t)
	tr := trayAtPresets(f.start(t))
	tr.lastWindowRows[0].Selected, tr.lastWindowRows[1].Selected = false, true

	tr.onSelectWindow(context.Background(), 0)

	if stub.calls != 0 {
		t.Errorf("dialogs asked = %d, want none on the way back to 200K", stub.calls)
	}
	if got := f.snapshot(); len(got) != 1 || got[0].ContextWindow != hostfit.ServingWindow200k {
		t.Errorf("posts = %+v, want the model in use at 200K", got)
	}
}

// No dialog backend: not consent, and not silent — as the unfit switch.
func TestOnSelectWindow_NoDialogBackendNamesTheCommand(t *testing.T) {
	f := &windowPostFake{}
	log := resetSeams(t)
	toasts := installStubNotifier(t)
	(&labelStub{confirmed: false, ok: false}).install(t)
	tr := trayAtPresets(f.start(t))

	tr.onSelectWindow(context.Background(), 1)

	if got := f.snapshot(); len(got) != 0 {
		t.Errorf("posts = %+v, want none — an unanswerable question is not consent", got)
	}
	calls := toasts.snapshot()
	if len(calls) != 1 || !strings.Contains(calls[0].body, "waired models use reaches --window 1m") ||
		calls[0].level != notification.Warning {
		t.Errorf("toasts = %+v, want one warning naming the command", calls)
	}
	if got := log.snapshot(&log.clipboard); len(got) != 1 || got[0] != "waired models use reaches --window 1m" {
		t.Errorf("clipboard = %v, want the command ready to paste", got)
	}
}

// The tray says what the console says. A drift here would show a person two
// different warnings for one choice.
func TestLongWindowPrompt_IsTheConsoleCopy(t *testing.T) {
	title, body := longWindowPrompt()
	if title != "1M runs models past what they were trained for" {
		t.Errorf("title %q", title)
	}
	for _, want := range []string{
		"from 262,144 to 1,048,576 tokens",
		"short prompts may be affected",
		"has been measured",
		"can take hours",
		"Use the 1M context window on this computer?",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q: %q", want, body)
		}
	}
}

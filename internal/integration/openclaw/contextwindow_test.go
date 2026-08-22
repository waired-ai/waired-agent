package openclaw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
)

// modelsBody is what the data-plane gateway serves at /v1/models.
const modelsBody = `{"object":"list","data":[
  {"id":"waired/default","object":"model","created":1,"owned_by":"waired","max_input_tokens":200704},
  {"id":"qwen3.5-35b-a3b","object":"model","created":1,"owned_by":"waired","max_input_tokens":131072}
]}`

// TestFetchContextWindow_ReadsTheServedWindow exercises the real fetch against
// a real listener rather than a seam, so the URL it builds and the field it
// reads are both covered (CLAUDE.md §Test discipline).
func TestFetchContextWindow_ReadsTheServedWindow(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(modelsBody))
	}))
	defer srv.Close()

	if got := fetchContextWindow(context.Background(), srv.URL, "waired/default"); got != 200704 {
		t.Errorf("fetchContextWindow = %d, want 200704", got)
	}
	if gotPath != "/v1/models" {
		t.Errorf("fetched %q, want /v1/models", gotPath)
	}
	// A trailing slash on the base URL must not produce //v1/models.
	if got := fetchContextWindow(context.Background(), srv.URL+"/", "qwen3.5-35b-a3b"); got != 131072 {
		t.Errorf("with a trailing slash = %d, want 131072", got)
	}
	if gotPath != "/v1/models" {
		t.Errorf("fetched %q after a trailing slash, want /v1/models", gotPath)
	}
}

// TestFetchContextWindow_UnknownIsZeroNotAnError: every way this comes up
// empty has to be "not known", because a link must not fail over it and the
// plugin must not be handed a number nobody stands behind.
func TestFetchContextWindow_UnknownIsZeroNotAnError(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"404": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNotFound) },
		"500": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
		"not json": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>nope"))
		},
		"older daemon: no such field": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"waired/default","object":"model"}]}`))
		},
		"model not listed": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"something-else","max_input_tokens":4096}]}`))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			if got := fetchContextWindow(context.Background(), srv.URL, "waired/default"); got != 0 {
				t.Errorf("fetchContextWindow = %d, want 0", got)
			}
		})
	}

	// Nothing listening at all — the wizard applies integrations before the
	// gateway serves, and `waired link` runs on a host whose agent may be down.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if got := fetchContextWindow(context.Background(), url, "waired/default"); got != 0 {
		t.Errorf("with nothing listening = %d, want 0", got)
	}

	if got := fetchContextWindow(context.Background(), "", "waired/default"); got != 0 {
		t.Errorf("with an empty base URL = %d, want 0", got)
	}
}

// TestRenderEntry_DeclaresTheWindowItWasGiven is the #1001 regression: the
// template used to carry a literal 32768 for every host and every model.
func TestRenderEntry_DeclaresTheWindowItWasGiven(t *testing.T) {
	body, err := renderEntry("http://127.0.0.1:9473", 200704)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "const CONTEXT_WINDOW = 200704;") {
		t.Errorf("rendered plugin does not declare the window it was given:\n%s", s)
	}
	if strings.Contains(s, "32768") {
		t.Errorf("rendered plugin still carries the old constant:\n%s", s)
	}
}

// TestRenderEntry_UnknownWindowIsNotDeclared: 0 means "not known", and the
// plugin then has to leave the field off so OpenClaw uses its own default
// rather than a figure waired invented.
func TestRenderEntry_UnknownWindowIsNotDeclared(t *testing.T) {
	body, err := renderEntry("http://127.0.0.1:9473", 0)
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "const CONTEXT_WINDOW = 0;") {
		t.Errorf("expected CONTEXT_WINDOW = 0:\n%s", s)
	}
	if !strings.Contains(s, "if (CONTEXT_WINDOW > 0)") {
		t.Errorf("plugin has no guard, so it would declare contextWindow: 0:\n%s", s)
	}
	// A negative window is a caller bug, not a value to render.
	body, err = renderEntry("http://127.0.0.1:9473", -5)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "const CONTEXT_WINDOW = 0;") {
		t.Errorf("a negative window must clamp to 0:\n%s", body)
	}
}

// TestApply_BakesTheFetchedWindowIntoThePlugin covers the wiring the seam
// hides: Apply must ask for the window of the model the plugin actually
// names, at the data-plane URL, and put the answer in the file.
func TestApply_BakesTheFetchedWindowIntoThePlugin(t *testing.T) {
	var gotBase, gotModel string
	prev := contextWindowFn
	contextWindowFn = func(_ context.Context, base, model string) int {
		gotBase, gotModel = base, model
		return 200704
	}
	t.Cleanup(func() { contextWindowFn = prev })

	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if want := DataPlaneBaseURL(opts.GatewayBaseURL); gotBase != want {
		t.Errorf("asked %q for the window, want the data-plane URL %q", gotBase, want)
	}
	if gotModel != modelRefs()[0] {
		t.Errorf("asked about %q, want the ref the plugin declares (%q)", gotModel, modelRefs()[0])
	}
	entry, err := os.ReadFile(PluginEntryFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(entry), "const CONTEXT_WINDOW = 200704;") {
		t.Errorf("plugin does not carry the fetched window:\n%s", entry)
	}
}

// TestApply_UnknownWindowStillLinks: an agent that is not serving yet must not
// turn a link into a failure.
func TestApply_UnknownWindowStillLinks(t *testing.T) {
	prev := contextWindowFn
	contextWindowFn = func(context.Context, string, string) int { return 0 }
	t.Cleanup(func() { contextWindowFn = prev })

	a := New()
	opts := newOpts(t)
	if err := a.Apply(context.Background(), opts); err != nil {
		t.Fatalf("Apply with no resolvable window: %v", err)
	}
	entry, err := os.ReadFile(PluginEntryFile(opts.HomeDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(entry), "const CONTEXT_WINDOW = 0;") {
		t.Errorf("plugin should declare no window:\n%s", entry)
	}
	if _, ok := loadRec(t, opts); !ok {
		t.Error("no ledger record written")
	}
}

var _ = integration.AgentOpenClaw

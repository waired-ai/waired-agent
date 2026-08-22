package tray

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// What a daemon that predates waired-agent#986 answers, and what this tray
// makes of it. Both bodies are verbatim from a live 0.0.3~rc3 Linux
// system-service install, read over its management socket.
//
// Record of today's behaviour, not a product contract: the skew window is
// one edge update wide and deliberately ungated (pre-release). It is pinned
// because the two endpoints degrade DIFFERENTLY, and the first write-up of
// this decision got it wrong in one direction — worth a test rather than a
// second guess.
const (
	rc3OpenClawBody = `{"config":{"path":"/var/lib/waired/.openclaw/plugins/waired/index.mjs","flavor":"openclaw","configured":false,"stale":false}}`
)

func TestOlderDaemonBodyDegradesToNoDriftDetection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/waired/v1/integration/openclaw" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(rc3OpenClawBody))
			return
		}
		// The same daemon has no opencode endpoint at all.
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := newTestClient(srv.URL)

	// Served, but in the old shape: the decoder drops what it does not know,
	// so this is not an error — it is an EMPTY expectation.
	ow, err := c.OpenClawIntegration(context.Background())
	if err != nil {
		t.Fatalf("OpenClawIntegration against an older daemon: %v", err)
	}
	if ow.ExpectedBaseURL != "" {
		t.Errorf("ExpectedBaseURL = %q, want empty from a body that carries none", ow.ExpectedBaseURL)
	}

	// An empty expectation disables staleness detection (internal/integration/
	// detect: "Empty disables staleness detection"), but the row is still
	// read from THIS user's home — which is the whole point of #986. So the
	// answer stays true, it just cannot notice drift.
	home := t.TempDir()
	writeOpenClawPlugin(t, home, "http://127.0.0.1:9479/v1")
	got := probeOpenClaw(home, ow.ExpectedBaseURL)
	if got == nil || !got.Configured {
		t.Fatalf("row = %+v, want configured", got)
	}
	if got.Stale {
		t.Errorf("row = %+v, want no staleness verdict without an expectation", got)
	}

	m := MenuModel{Kind: MenuConnected}
	applyOpenClaw(&m, got)
	if m.OpenClawHeader != "OpenClaw integration: ● configured" {
		t.Errorf("header = %q", m.OpenClawHeader)
	}

	// The endpoint the older daemon does not have degrades the other way:
	// unsupported, so Update() hides the group rather than guessing.
	if _, ocErr := c.OpenCodeIntegration(context.Background()); ocErr != ErrOpenCodeIntegrationUnsupported {
		t.Errorf("OpenCodeIntegration error = %v, want the unsupported sentinel", ocErr)
	}
}

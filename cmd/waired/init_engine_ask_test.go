package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func eofLineReader() lineReader { return bufio.NewScanner(strings.NewReader("")) }

func linesOf(s string) lineReader { return bufio.NewScanner(strings.NewReader(s)) }

// TestEngineInstallAsk pins the step-4 precedence as the product
// contract from the 2026-08-08 owner rulings (waired-ai/waired#1067;
// waired-agent#584): explicit flag > non-interactive default >
// interactive ask.
func TestEngineInstallAsk(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		forced, nonInteractive, fit bool
		want                        engineAskAnswer
	}{
		{"flag wins on a fit host", true, false, true, engineAskInstall},
		{"flag wins on an unfit host", true, false, false, engineAskInstall},
		{"flag wins even non-interactively", true, true, false, engineAskInstall},
		{"interactive fit host is asked", false, false, true, engineAskPrompt},
		{"interactive unfit host is asked", false, false, false, engineAskPrompt},
		{"non-interactive fit host installs", false, true, true, engineAskInstall},
		{"non-interactive unfit host skips", false, true, false, engineAskSkip},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := engineInstallAsk(tc.forced, tc.nonInteractive, tc.fit); got != tc.want {
				t.Errorf("engineInstallAsk(%v, %v, %v) = %v, want %v",
					tc.forced, tc.nonInteractive, tc.fit, got, tc.want)
			}
		})
	}
}

// askFakeDaemon serves the two routes step 4 touches: the catalog read
// and the disable write, recording the latter.
type askFakeDaemon struct {
	catalog  catalogDetailResp
	noCat    bool
	disables atomic.Int32
}

func (f *askFakeDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/waired/v1/inference/catalog":
			if f.noCat {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(f.catalog)
		case "/waired/v1/inference/disable":
			f.disables.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fitCatalog() catalogDetailResp {
	var c catalogDetailResp
	c.Families = []catalogDetailFamily{
		{ModelID: "qwen3.5-9b", DisplayName: "Qwen3.5 9B", Fits: true, RecommendedPick: true},
		{ModelID: "qwen3.5-27b", Fits: false},
	}
	return c
}

func unfitCatalog(anyFits bool) catalogDetailResp {
	var c catalogDetailResp
	c.Families = []catalogDetailFamily{{ModelID: "qwen3.5-0.8b", Fits: anyFits}}
	return c
}

// TestConfirmDaemonPathEngineInstall covers the interactive arms of
// step 4 with the approved copy (owner approval 2026-08-09, this
// session): fit → default Yes, unfit → warning + default No, decline →
// disable POSTed and the gateway/relay note printed.
func TestConfirmDaemonPathEngineInstall(t *testing.T) {
	boolp := func(b bool) *bool { return &b }

	t.Run("fit host defaults to install", func(t *testing.T) {
		f := &askFakeDaemon{catalog: fitCatalog()}
		var out strings.Builder
		got := confirmDaemonPathEngineInstall(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out)
		if !got || f.disables.Load() != 0 {
			t.Fatalf("= %v (disables %d), want install with no disable", got, f.disables.Load())
		}
		if !strings.Contains(out.String(), "recommended: Qwen3.5 9B") ||
			!strings.Contains(out.String(), "Run AI models on this computer?") {
			t.Errorf("prompt missing the recommendation or the question: %q", out.String())
		}
		if !strings.Contains(out.String(), "(default: Yes)") {
			t.Errorf("fit host must default Yes: %q", out.String())
		}
	})

	t.Run("fit host can decline", func(t *testing.T) {
		f := &askFakeDaemon{catalog: fitCatalog()}
		var out strings.Builder
		got := confirmDaemonPathEngineInstall(f.server(t).URL, daemonInitInference{}, false, linesOf("n\n"), &out)
		if got || f.disables.Load() != 1 {
			t.Fatalf("= %v (disables %d), want a decline recorded once", got, f.disables.Load())
		}
		if !strings.Contains(out.String(), "Skipping local AI — Waired keeps working as a gateway/relay.") ||
			!strings.Contains(out.String(), "`waired inference on`") {
			t.Errorf("decline note missing: %q", out.String())
		}
	})

	t.Run("unfit host warns and defaults to no", func(t *testing.T) {
		f := &askFakeDaemon{catalog: unfitCatalog(false)}
		var out strings.Builder
		got := confirmDaemonPathEngineInstall(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out)
		if got || f.disables.Load() != 1 {
			t.Fatalf("= %v (disables %d), want the default decline", got, f.disables.Load())
		}
		if !strings.Contains(out.String(), "below the recommended spec for local AI: no bundled model fits in this computer's memory.") ||
			!strings.Contains(out.String(), "(default: No)") {
			t.Errorf("unfit warning or default missing: %q", out.String())
		}
	})

	t.Run("unfit host can still opt in", func(t *testing.T) {
		f := &askFakeDaemon{catalog: unfitCatalog(true)}
		var out strings.Builder
		got := confirmDaemonPathEngineInstall(f.server(t).URL, daemonInitInference{}, false, linesOf("y\n"), &out)
		if !got || f.disables.Load() != 0 {
			t.Fatalf("= %v (disables %d), want an explicit yes to install", got, f.disables.Load())
		}
		if !strings.Contains(out.String(), "Waired would not choose any of them") {
			t.Errorf("fits-but-not-recommended reason missing: %q", out.String())
		}
	})

	t.Run("explicit flag asks nothing", func(t *testing.T) {
		f := &askFakeDaemon{catalog: unfitCatalog(false)}
		var out strings.Builder
		got := confirmDaemonPathEngineInstall(f.server(t).URL,
			daemonInitInference{Enabled: boolp(true)}, false, eofLineReader(), &out)
		if !got || out.Len() != 0 {
			t.Fatalf("= %v out=%q, want a silent install under --inference-enabled=true", got, out.String())
		}
	})

	t.Run("non-interactive unfit host skips with the reason", func(t *testing.T) {
		f := &askFakeDaemon{catalog: unfitCatalog(false)}
		var out strings.Builder
		got := confirmDaemonPathEngineInstall(f.server(t).URL, daemonInitInference{}, true, eofLineReader(), &out)
		if got || f.disables.Load() != 1 {
			t.Fatalf("= %v (disables %d), want the non-interactive skip", got, f.disables.Load())
		}
		if !strings.Contains(out.String(), "Non-interactive: skipping local AI") {
			t.Errorf("non-interactive note missing: %q", out.String())
		}
	})

	t.Run("an older daemon without the catalog is still asked, defaulting yes", func(t *testing.T) {
		f := &askFakeDaemon{noCat: true}
		var out strings.Builder
		got := confirmDaemonPathEngineInstall(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out)
		if !got {
			t.Fatal("want the safe default install when the catalog cannot ground a warning")
		}
		if strings.Contains(out.String(), "recommended:") || strings.Contains(out.String(), "below the recommended spec") {
			t.Errorf("no recommendation data, so neither clause may print: %q", out.String())
		}
	})
}

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

// eofLineReader is a stdin that ends before the first read: nobody is at
// the keyboard. enterLineReader is a person pressing Enter.
//
// They are two different things, and until waired-agent#1048 nothing in
// the product could tell them apart — ynPrompt mapped both to the
// default — so the tests below used eofLineReader wherever they meant
// Enter. Every one of those has moved to enterLineReader, which is what
// their contract always said; the EOF rows are new and assert the other
// answer. Both are still needed: a few call sites deliberately never
// reach a prompt, and eofLineReader proves they did not.
func eofLineReader() lineReader { return bufio.NewScanner(strings.NewReader("")) }

func enterLineReader() lineReader { return linesOf("\n") }

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
		got := confirmDaemonPathEngineInstall(f.server(t).URL, daemonInitInference{}, false, enterLineReader(), &out)
		if !got || f.disables.Load() != 0 {
			t.Fatalf("= %v (disables %d), want install with no disable", got, f.disables.Load())
		}
		if !strings.Contains(out.String(), "This computer can run models locally. You choose which model in a moment.") ||
			!strings.Contains(out.String(), "Run models on this computer?") {
			t.Errorf("prompt missing the fit sentence or the question: %q", out.String())
		}
		// PRODUCT CONTRACT (waired-agent#649): step 4 names no model. The
		// recommendation it could compute here is taken before the engine
		// exists, so it can differ from the picker's — which is exactly what
		// the issue observed. This test inverts the previous assertion, which
		// required the name to be printed.
		if strings.Contains(out.String(), "Qwen3.5 9B") || strings.Contains(out.String(), "recommended:") {
			t.Errorf("step 4 named a model: %q", out.String())
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
		if !strings.Contains(out.String(), "Skipping local inference — Waired keeps working as a gateway/relay.") ||
			!strings.Contains(out.String(), "`waired inference on`") {
			t.Errorf("decline note missing: %q", out.String())
		}
	})

	// PRODUCT CONTRACT (waired-agent#1048): an exhausted stdin is not the
	// Enter above it. This is the row the fit host got until now — the
	// question printed, nobody answered, and the default installed an
	// engine and started a multi-GB download.
	//
	// The host lands exactly where a typed "n" leaves it, in the same
	// words, because that is the same outcome: no engine, local
	// inference off, exit 0. Only the line naming the reason differs.
	t.Run("stdin ends before the question is answered", func(t *testing.T) {
		f := &askFakeDaemon{catalog: fitCatalog()}
		var out strings.Builder
		got := confirmDaemonPathEngineInstall(f.server(t).URL, daemonInitInference{}, false, eofLineReader(), &out)
		if got || f.disables.Load() != 1 {
			t.Fatalf("= %v (disables %d), want a skip recorded once", got, f.disables.Load())
		}
		for _, want := range []string{
			"No answer on stdin — nobody is here to say whether this computer should run models.",
			"Skipping local inference — Waired keeps working as a gateway/relay.",
			"Turn it on anytime with `waired inference on`.",
		} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("output missing %q: %q", want, out.String())
			}
		}
		// --non-interactive is a different statement and keeps its own
		// wording: on a FIT host that flag installs, so borrowing its
		// line here would name a mode that would have done the opposite.
		if strings.Contains(out.String(), "Non-interactive:") {
			t.Errorf("a closed pipe is not --non-interactive: %q", out.String())
		}
	})

	t.Run("unfit host warns and defaults to no", func(t *testing.T) {
		f := &askFakeDaemon{catalog: unfitCatalog(false)}
		var out strings.Builder
		got := confirmDaemonPathEngineInstall(f.server(t).URL, daemonInitInference{}, false, enterLineReader(), &out)
		if got || f.disables.Load() != 1 {
			t.Fatalf("= %v (disables %d), want the default decline", got, f.disables.Load())
		}
		if !strings.Contains(out.String(), "below the recommended spec for local inference: no bundled model fits in this computer's memory.") ||
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
		if !strings.Contains(out.String(), "none of them is recommended") {
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
		if !strings.Contains(out.String(), "Non-interactive: skipping local inference") {
			t.Errorf("non-interactive note missing: %q", out.String())
		}
	})

	t.Run("an older daemon without the catalog is still asked, defaulting yes", func(t *testing.T) {
		f := &askFakeDaemon{noCat: true}
		var out strings.Builder
		got := confirmDaemonPathEngineInstall(f.server(t).URL, daemonInitInference{}, false, enterLineReader(), &out)
		if !got {
			t.Fatal("want the safe default install when the catalog cannot ground a warning")
		}
		if strings.Contains(out.String(), "below the recommended spec") {
			t.Errorf("no catalog data, so no warning may print: %q", out.String())
		}
	})
}

// TestStep4AndThePickerAgreeOnTheRecommendation is the regression pin for
// waired-agent#649: the wizard named one model at step 4 and a different
// one in the picker seconds later, because the same daemon-side
// recommendation is computed before and after the engine install and the
// engine version changes which variants qualify.
//
// PRODUCT CONTRACT (waired-agent#649, owner decision 2026-08-11): one
// surface names the model, and it is the picker — the one that asks after
// the facts are in. So the pin is that step 4 names nothing at all, which
// is what makes the two unable to disagree.
func TestStep4AndThePickerAgreeOnTheRecommendation(t *testing.T) {
	// The two catalogs the issue actually saw on one host: before the
	// engine install the floored variants are excluded and the dense 27B
	// wins; after it, the 35B-A3B does.
	before := catalogDetailResp{Families: []catalogDetailFamily{
		{ModelID: "qwen3.6-27b", DisplayName: "Qwen3.6 27B", Fits: true, RecommendedPick: true},
		{ModelID: "qwen3.6-35b-a3b", DisplayName: "Qwen3.6 35B-A3B", Fits: true},
	}}
	after := catalogDetailResp{Families: []catalogDetailFamily{
		{ModelID: "qwen3.6-27b", DisplayName: "Qwen3.6 27B", Fits: true},
		{ModelID: "qwen3.6-35b-a3b", DisplayName: "Qwen3.6 35B-A3B", Fits: true, RecommendedPick: true},
	}}

	f := &askFakeDaemon{catalog: before}
	var step4 strings.Builder
	if !confirmDaemonPathEngineInstall(f.server(t).URL, daemonInitInference{}, false, enterLineReader(), &step4) {
		t.Fatal("a fit host must default to installing")
	}
	for _, name := range []string{"Qwen3.6 27B", "qwen3.6-27b", "Qwen3.6 35B-A3B", "qwen3.6-35b-a3b"} {
		if strings.Contains(step4.String(), name) {
			t.Errorf("step 4 named %q; only the picker names a model: %q", name, step4.String())
		}
	}

	var list strings.Builder
	def := renderModelPickerList(&list, after)
	if def != 2 {
		t.Errorf("picker default = %d, want the recommended row (2)", def)
	}
	if !strings.Contains(list.String(), "Qwen3.6 35B-A3B — recommended for this computer") {
		t.Errorf("picker row missing the display name: %q", list.String())
	}
	// The id spelling is what step 4 was being compared against; the row
	// now reads like every other surface that names a model.
	if strings.Contains(list.String(), "qwen3.6-35b-a3b —") {
		t.Errorf("picker row still names the model by id: %q", list.String())
	}
}

package main

import (
	"strings"
	"testing"
)

// TestStatusSkipsAnEngineThatIsNotInstalled exercises a branch that had
// never once fired.
//
// `waired status` has skipped uninstalled runtimes since it was written
// (`if !r.Installed { continue }`), but the daemon set Installed to the
// literal true for every registered adapter, so the branch was dead and
// the summary listed an engine that was not on the host. #852 makes the
// daemon answer honestly; this pins that the skip now happens, because a
// guard nobody reaches is indistinguishable from one that works.
func TestStatusSkipsAnEngineThatIsNotInstalled(t *testing.T) {
	const notInstalled = `{"subsystem_state":"no_engine","runtimes":{` +
		`"ollama":{"name":"ollama","installed":false,"state":"not_started"}}}`
	out := captureStdout(t, func() { printInferenceSummary([]byte(notInstalled)) })
	if strings.Contains(out, "runtimes:") {
		t.Errorf("an engine reported not installed is listed as a runtime:\n%s", out)
	}
	if !strings.Contains(out, "no_engine") {
		t.Errorf("the subsystem state is missing from the summary:\n%s", out)
	}

	const installed = `{"subsystem_state":"ready","runtimes":{` +
		`"ollama":{"name":"ollama","installed":true,"state":"ready","version":"0.32.13"}}}`
	out = captureStdout(t, func() { printInferenceSummary([]byte(installed)) })
	if !strings.Contains(out, "runtimes:") || !strings.Contains(out, "0.32.13") {
		t.Errorf("an installed engine is not listed:\n%s", out)
	}
}

// TestCatalogHeaderSaysWhenNoEngineIsInstalled pins the other half of
// #852. catalogEngine names the engine this host WOULD use, falling back
// to the auto-picker when nothing is committed, so a host with no engine
// at all still gets "ollama" — and every row was rendered as a verdict
// by that engine with nothing anywhere saying it was absent.
//
// The verdicts stay (they are true about what this computer would run
// once an engine is here, and the catalog is also a browse surface);
// what is added is the context, plus both halves of the truth: no engine
// here, and the requests go to the other computers (waired-agent#387,
// #841).
func TestCatalogHeaderSaysWhenNoEngineIsInstalled(t *testing.T) {
	base := catalogDetailResp{
		Engine: "ollama",
		Host:   catalogDetailHost{RAMTotalGB: 63, GPUModel: "Intel Arc", VRAMTotalMB: 8192},
		Families: []catalogDetailFamily{{
			ModelID: "qwen3.8-27b", DisplayName: "Qwen3.8 27B", Fits: true,
		}},
	}
	no, yes := false, true

	t.Run("no engine installed", func(t *testing.T) {
		c := base
		c.EngineInstalled = &no
		out := formatCatalogDetail(c)
		if strings.Contains(out, "engine=ollama") {
			t.Errorf("the header names an engine that is not installed:\n%s", out)
		}
		for _, want := range []string{
			"no AI engine installed",
			"cannot run a model itself",
			"Requests go to your other computers instead.",
			"waired runtimes install ollama",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("header is missing %q:\n%s", want, out)
			}
		}
		// The rows are the point of keeping the verdicts.
		if !strings.Contains(out, "qwen3.8-27b") {
			t.Errorf("the model rows disappeared on an engine-less host:\n%s", out)
		}
	})

	t.Run("engine installed", func(t *testing.T) {
		c := base
		c.EngineInstalled = &yes
		out := formatCatalogDetail(c)
		if !strings.Contains(out, "engine=ollama") {
			t.Errorf("the ordinary header is gone:\n%s", out)
		}
		if strings.Contains(out, "no AI engine") {
			t.Errorf("an installed engine is described as missing:\n%s", out)
		}
	})

	t.Run("daemon predates the field", func(t *testing.T) {
		// nil is unknown, not absent: an older daemon must render
		// exactly what it rendered before the field existed.
		out := formatCatalogDetail(base)
		if !strings.Contains(out, "engine=ollama") {
			t.Errorf("a daemon without engine_installed lost its header:\n%s", out)
		}
		if strings.Contains(out, "no AI engine") {
			t.Errorf("unknown was rendered as absent:\n%s", out)
		}
	})
}

// TestModelPickerSaysWhenNoEngineIsInstalled covers the wizard's model
// list. The engine question is asked EARLIER in the install flow, so
// this does not re-offer the install; it says what the list is about on
// a computer that will not run any of it itself.
func TestModelPickerSaysWhenNoEngineIsInstalled(t *testing.T) {
	base := catalogDetailResp{
		Engine: "ollama",
		Families: []catalogDetailFamily{{
			ModelID: "qwen3.8-27b", DisplayName: "Qwen3.8 27B", Fits: true, RecommendedPick: true,
		}},
	}
	no, yes := false, true

	render := func(c catalogDetailResp) string {
		var b strings.Builder
		renderModelPickerList(&b, c)
		return b.String()
	}

	out := render(func() catalogDetailResp { c := base; c.EngineInstalled = &no; return c }())
	for _, want := range []string{
		"No AI engine is installed on this computer",
		"requests go to your other computers",
		"if you add an engine later",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("picker is missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "Qwen3.8 27B") {
		t.Errorf("the rows disappeared:\n%s", out)
	}

	for name, c := range map[string]catalogDetailResp{
		"engine installed":          func() catalogDetailResp { c := base; c.EngineInstalled = &yes; return c }(),
		"daemon predates the field": base,
	} {
		if strings.Contains(render(c), "No AI engine") {
			t.Errorf("%s: the picker claims there is no engine", name)
		}
	}
}

// TestEngineInstallSentenceQuotesOnlyTheCommand pins #852's Windows
// rendering. `waired models ls --detail` printed
//
//	Install one with `waired runtimes install ollama (from an elevated prompt)`.
//
// on pc-dell-premium — the backticks promised a command and delivered a
// command plus a sentence. The elevation is still said; it is said
// outside the quotes.
func TestEngineInstallSentenceQuotesOnlyTheCommand(t *testing.T) {
	for goos, want := range map[string]string{
		"linux":   "Install one with `sudo waired runtimes install ollama`.",
		"darwin":  "Install one with `sudo waired runtimes install ollama`.",
		"windows": "Install one with `waired runtimes install ollama`, from an elevated prompt.",
	} {
		if got := engineInstallSentence(goos); got != want {
			t.Errorf("%s: %q, want %q", goos, got, want)
		}
	}
}

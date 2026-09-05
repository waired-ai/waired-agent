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

// THE #1107 BAR. PRODUCT CONTRACT: a runtime row that carries a reason
// gets its warning printed, installed or not.
//
// The skip above is right for the `runtimes:` version list, and it took
// the ⚠ line with it. #1075 synthesises a row for a bootstrap that refused
// before it built an adapter, and sets installed from what is actually on
// the host — so the most common refusal, "the vllm venv is not active",
// produces installed=false by construction. The reason travelled onto the
// wire correctly and `waired status` dropped it on exactly the hosts that
// had one. `waired runtimes ls` never had this skip.
func TestStatusWarnsAboutAnUninstalledEngineThatSaidWhy(t *testing.T) {
	const refused = `{"subsystem_state":"engine_failed","runtimes":{` +
		`"vllm":{"name":"vllm","installed":false,"state":"failed",` +
		`"last_error":"vllm venv not active under /var/lib/waired (run 'waired runtimes install vllm')"}}}`
	out := captureStdout(t, func() { printInferenceSummary([]byte(refused)) })

	if !strings.Contains(out, "vllm venv not active") {
		t.Errorf("the engine said why it refused and the summary dropped it:\n%s", out)
	}
	// Still not in the version list: "not installed" is a fair reason to
	// leave it out of a list of what is installed.
	if strings.Contains(out, "runtimes:") {
		t.Errorf("an engine reported not installed is listed as a runtime:\n%s", out)
	}
}

// The line an operator reads must not depend on Go's map iteration order —
// the same rule engineFailureDetail states for itself, which this loop had
// never applied.
func TestStatusWarningsAreInAStableOrder(t *testing.T) {
	const two = `{"subsystem_state":"engine_failed","runtimes":{` +
		`"vllm":{"name":"vllm","installed":true,"state":"failed","last_error":"vllm reason"},` +
		`"ollama":{"name":"ollama","installed":true,"state":"failed","last_error":"ollama reason"}}}`
	// One pass can agree with a sorted order by luck; the assertion is
	// that every pass does.
	for range 50 {
		out := captureStdout(t, func() { printInferenceSummary([]byte(two)) })
		o, v := strings.Index(out, "ollama reason"), strings.Index(out, "vllm reason")
		if o < 0 || v < 0 {
			t.Fatalf("both reasons must be reported:\n%s", out)
		}
		if o > v {
			t.Fatalf("warnings came out vllm-before-ollama; the order must not depend on\n"+
				"map iteration:\n%s", out)
		}
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
			"no inference engine installed",
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
		if strings.Contains(out, "no inference engine") {
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
		if strings.Contains(out, "no inference engine") {
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
		"No inference engine is installed on this computer",
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
		if strings.Contains(render(c), "No inference engine") {
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

// PRODUCT CONTRACT (waired-agent#1140): the STATE word tells a stop
// somebody asked for from one the engine decided on its own.
//
// A give-up latch reaches the wire as state "stopped" — Stop() overwrites
// the whole Health struct with no give-up guard, so a model switch or a
// reconcile bounce after the give-up leaves it there. "stopped" is the word
// a person gets after `waired inference engine stop`, so printing it here
// said the opposite of the ⚠ line two rows below it.
func TestStatusNamesAnEngineThatGaveUp(t *testing.T) {
	const latched = `{"subsystem_state":"engine_failed","runtimes":{` +
		`"ollama":{"name":"ollama","installed":true,"state":"stopped","version":"0.32.15",` +
		`"failure_latched":true,"last_error":"engine repeatedly crashed; not retrying"}}}`
	out := captureStdout(t, func() { printInferenceSummary([]byte(latched)) })
	if !strings.Contains(out, "gave up") {
		t.Errorf("a latched engine is listed as merely stopped:\n%s", out)
	}
	if !strings.Contains(out, "not retrying") {
		t.Errorf("the reason went missing:\n%s", out)
	}

	// The control: an engine somebody stopped still says stopped, so the
	// word above cannot become what every idle engine reports.
	const asked = `{"subsystem_state":"stopped","runtimes":{` +
		`"ollama":{"name":"ollama","installed":true,"state":"stopped","version":"0.32.15"}}}`
	out = captureStdout(t, func() { printInferenceSummary([]byte(asked)) })
	if strings.Contains(out, "gave up") {
		t.Errorf("an engine somebody stopped is reported as having given up:\n%s", out)
	}
	if !strings.Contains(out, "stopped") {
		t.Errorf("the stopped state went missing:\n%s", out)
	}
}

// TestStatusLeavesTheEngineWarningsToTheNoticeBlock
//
// PRODUCT CONTRACT (waired-agent#1229). The two engine warnings used to
// be appended to this loop; the daemon publishes them now, and they
// print under `Notices:` alongside the same two rows the tray and
// `waired doctor` show. This asserts they are not ALSO printed here,
// because one thing said twice in one command is the defect the move was
// meant to avoid.
//
// last_error stays: it is why the engine is not running, which is state
// rather than advice, and the loop still reports it.
func TestStatusLeavesTheEngineWarningsToTheNoticeBlock(t *testing.T) {
	const both = `{"subsystem_state":"ready","runtimes":{` +
		`"ollama":{"name":"ollama","installed":true,"state":"ready","version":"0.33.2",` +
		`"version_warning":"engine version 0.24.0 does not match the bundled pin 0.33.2",` +
		`"tuning_warning":"model spills to system RAM even at the minimum context window",` +
		`"last_error":"the engine could not bind 127.0.0.1:9479"}}}`
	out := captureStdout(t, func() { printInferenceSummary([]byte(both)) })
	if strings.Contains(out, "does not match the bundled pin") {
		t.Errorf("the version warning is printed here as well as in the notice block:\n%s", out)
	}
	if strings.Contains(out, "spills to system RAM") {
		t.Errorf("the tuning warning is printed here as well as in the notice block:\n%s", out)
	}
	if !strings.Contains(out, "could not bind") {
		t.Errorf("last_error is engine state and must still be reported:\n%s", out)
	}
}

package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
)

// forceFoldForTest pins foldOutput for one test, so the guard over the init
// renderers can run the Windows-console arm on any CI runner.
func forceFoldForTest(t *testing.T, on bool) {
	t.Helper()
	prev := foldCached
	foldCached = &on
	t.Cleanup(func() { foldCached = prev })
}

// TestAsciiOnlySink pins which sinks are folded. Product contract
// (waired-ai/waired-agent#629, extended by #1105): output is folded exactly
// where something between this process and the reader decodes the bytes with a
// table that cannot hold them.
//
// On Windows that is the console output code page, and PowerShell reading a
// native command's stdout uses the encoding it cached at startup — so a console
// and a redirect are both at risk when the page is not CP_UTF8.
//
// On Unix a pipe is byte-transparent and stays unfolded; the consumer picks the
// encoding. A terminal is not: it decodes by the locale, which is the same
// lossy step, so a terminal whose locale is not UTF-8 is folded.
//
// That last row used to be `false`, and #1105 inverts it. The argument for
// exempting Unix wholesale was written about pipes and is still right about
// pipes; the terminal was swept in with it, which left `LANG=C` with the emoji
// turned off (useEmoji does read the locale) and the em dashes raw — half a
// fallback, which is the shape #629 was about.
func TestAsciiOnlySink(t *testing.T) {
	tests := []struct {
		name  string
		goos  string
		facts glyphFacts
		want  bool
	}{
		{"windows CP932 console", "windows",
			glyphFacts{consoleUTF8: false, stdoutTTY: true}, true},
		{"windows CP_UTF8 console", "windows",
			glyphFacts{consoleUTF8: true, stdoutTTY: true}, false},
		// The rc8 capture: a pipe, on a host whose console this process has
		// since put on CP_UTF8. PowerShell decodes the pipe with the encoding
		// it cached at startup, so the code page we set does not save it.
		{"windows pipe off a CP_UTF8 console", "windows",
			glyphFacts{consoleUTF8: true}, true},
		{"windows redirect, no console", "windows", glyphFacts{}, true},
		// A Unix pipe is byte-transparent whatever the locale says: the
		// locale describes this terminal, and there isn't one.
		{"linux pipe", "linux", glyphFacts{}, false},
		{"linux pipe, C locale", "linux", glyphFacts{locale: "C"}, false},
		{"darwin pipe", "darwin", glyphFacts{}, false},
		// A Unix terminal decodes by the locale, so the locale decides.
		{"linux terminal", "linux", glyphFacts{locale: "en_US.UTF-8", stdoutTTY: true}, false},
		{"darwin terminal", "darwin", glyphFacts{locale: "en_US.UTF-8", stdoutTTY: true}, false},
		{"linux C locale terminal", "linux", glyphFacts{locale: "C", stdoutTTY: true}, true},
		{"darwin POSIX locale terminal", "darwin", glyphFacts{locale: "POSIX", stdoutTTY: true}, true},
		// No locale set at all is not UTF-8 either: firstSetLocale returns ""
		// when LC_ALL, LC_CTYPE and LANG are all empty, and a terminal on that
		// host decodes with whatever its own default is.
		{"linux terminal, no locale set", "linux", glyphFacts{stdoutTTY: true}, true},
		// WAIRED_NO_EMOJI turns glyphs off; it does not claim the sink is
		// byte-lossy, so it does not rewrite prose. The locale here is UTF-8
		// on purpose -- without it the row would pass for the other reason.
		{"linux WAIRED_NO_EMOJI", "linux",
			glyphFacts{noEmoji: true, locale: "en_US.UTF-8", stdoutTTY: true}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := asciiOnlySink(tt.goos, tt.facts); got != tt.want {
				t.Errorf("asciiOnlySink(%q, %+v) = %v, want %v", tt.goos, tt.facts, got, tt.want)
			}
		})
	}
}

func TestAsciiFold(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		// The two that produced the reported mojibake, from the plain branch
		// of welcomeBanner.
		{"Waired — connecting your coding agents to local inference…",
			"Waired - connecting your coding agents to local inference..."},
		{"Signed in — starting Waired on this device...",
			"Signed in - starting Waired on this device..."},
		// Frames and rules.
		{"╭──╮", "+--+"},
		{"│ x │", "| x |"},
		// Marks that some sites hardcode instead of routing through emo().
		{"⚠️ warning", "! warning"},
		{"✅ done", "* done"},
		{"a → b", "a -> b"},
		{"≥ 8 GB", ">= 8 GB"},
		// Non-ASCII that is data, not decoration, is left alone: a device
		// name, a user name and a model id can all be non-ASCII, and folding
		// those would be a worse bug than the one this fixes.
		{"デバイス: 開発機", "デバイス: 開発機"},
		{"C:\\Users\\山田\\AppData", "C:\\Users\\山田\\AppData"},
		// ASCII passes through untouched.
		{"plain ascii - already fine", "plain ascii - already fine"},
	}
	for _, tt := range tests {
		if got := asciiFold(tt.in); got != tt.want {
			t.Errorf("asciiFold(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestAsciiFoldLeavesNoVariationSelector guards the ordering trap in the
// Replacer table: "⚠" listed before "⚠️" would consume the base character and
// leave a bare U+FE0F behind, which is exactly the kind of stray byte #629 is
// about.
func TestAsciiFoldLeavesNoVariationSelector(t *testing.T) {
	for _, s := range []string{"⚠️", "⬇️", "ℹ️"} {
		got := asciiFold(s)
		if !isASCII(got) {
			t.Errorf("asciiFold(%q) = %q, which is still not ASCII (% x)", s, got, got)
		}
	}
}

// TestPlainRenderedOutputIsPureASCII is the regression guard #629 asks for.
//
// Product contract (waired-ai/waired-agent#629, owner ruling on
// waired-ai/waired#1127 L37): when the CLI has decided the sink cannot render
// glyphs, what it writes must be pure ASCII. Before this fix useEmoji() already
// returned false on every Windows console, but the strings on that path still
// carried em dashes and ellipses; a CP932 console decoded those UTF-8 bytes as
// Shift_JIS and the whole `waired init` transcript came out as mojibake.
//
// The fold is pinned on rather than inferred, so this runs the Windows arm on
// the Linux CI runner too — otherwise the guard would only ever execute on the
// one job that happens to be Windows.
//
// It was TestPlainInitOutputIsPureASCII until waired-agent#1105: the list was
// written for the `waired init` transcript and every renderer added since —
// doctor's findings, the model catalog — was outside it. This list is still
// written by hand, so it proves the renderings it names rather than the
// package; TestNonASCIILiteralsCanDegrade and
// TestPrintedOutputGoesThroughTheFold are what cover the package, by walking
// it instead of listing it.
func TestPlainRenderedOutputIsPureASCII(t *testing.T) {
	forceFoldForTest(t, true)
	resetGlyphCacheForTest()
	t.Setenv("WAIRED_NO_EMOJI", "1") // emoji off, as on a console that is not UTF-8
	t.Cleanup(resetGlyphCacheForTest)
	if useEmoji() {
		t.Fatal("useEmoji() = true; this test only means anything on the plain path")
	}

	speed := &management.HostSpeedStatus{}
	summary := daemonSummary{accountEmail: "you@example.com", claudeRouted: true}

	renderers := []struct {
		name string
		run  func(out *bytes.Buffer)
	}{
		{"welcomeBanner", func(o *bytes.Buffer) { welcomeBanner(o) }},
		{"rule", func(o *bytes.Buffer) { writePrompt(o, rule()) }},
		{"box", func(o *bytes.Buffer) {
			box(o, emo("✅", "*"), "Waired is ready — everything completed successfully!",
				[]string{dim("Signed in and running — this device is on your network.")})
		}},
		{"boxWarn", func(o *bytes.Buffer) {
			boxWarn(o, emo("⚠️", "!"), "Waired is signed in — local inference isn't running",
				[]string{dim("Watch it with: waired status")})
		}},
		{"printDaemonSettingUpBox", func(o *bytes.Buffer) {
			printDaemonSettingUpBox(o, "you@example.com", true)
		}},
		{"printDaemonNoModelBox", func(o *bytes.Buffer) {
			printDaemonNoModelBox(o, "you@example.com", true, nil)
		}},
		{"printDaemonTooSlowBox", func(o *bytes.Buffer) {
			printDaemonTooSlowBox(o, summary)
		}},
		{"printDaemonBenchmarkFailedBox", func(o *bytes.Buffer) {
			printDaemonBenchmarkFailedBox(o, "you@example.com", false)
		}},
		{"printDaemonEngineFailedBox", func(o *bytes.Buffer) {
			printDaemonEngineFailedBox(o, "you@example.com")
		}},
		{"printDaemonEngineOptOutBox", func(o *bytes.Buffer) {
			printDaemonEngineOptOutBox(o, "you@example.com", true)
		}},
		{"printDaemonEngineDownBox", func(o *bytes.Buffer) {
			printDaemonEngineDownBox(o, "you@example.com")
		}},
		{"printDaemonInferenceOffBox", func(o *bytes.Buffer) {
			printDaemonInferenceOffBox(o, daemonSummary{
				accountEmail: "you@example.com", claudeRouted: true, localInferenceOff: "disabled",
			})
		}},
		{"printDaemonInferenceOffBox/stopped", func(o *bytes.Buffer) {
			printDaemonInferenceOffBox(o, daemonSummary{
				accountEmail: "you@example.com", claudeRouted: true, localInferenceOff: "stopped",
			})
		}},
		{"printDaemonSuccessBox", func(o *bytes.Buffer) {
			printDaemonSuccessBox(o, "you@example.com", benchmarkOutcome{}, true, speed)
		}},
		{"printDaemonSuccessBox/named model", func(o *bytes.Buffer) {
			printDaemonSuccessBox(o, "you@example.com",
				benchmarkOutcome{Measured: true, Tokps: 13, ModelID: "qwen3.5-9b"}, true, speed)
		}},
		// The two surfaces waired-agent#1105 named. Both are string renderers
		// printed with fmt, so the fold reaches them through this package's
		// stdout rather than through writePrompt.
		{"formatFinding/ok", func(o *bytes.Buffer) {
			fmt.Fprintln(o, plainText(formatFinding(integration.AuditFinding{
				Status: integration.StatusOK, Subject: "device key", Detail: "present"})))
		}},
		{"formatFinding/warn", func(o *bytes.Buffer) {
			fmt.Fprintln(o, plainText(formatFinding(integration.AuditFinding{
				Status: integration.StatusWarn, Subject: "mesh peers", Detail: "2/3 reachable"})))
		}},
		{"formatFinding/fail", func(o *bytes.Buffer) {
			fmt.Fprintln(o, plainText(formatFinding(integration.AuditFinding{
				Status: integration.StatusFail, Subject: "device sign-in", Detail: "expired"})))
		}},
		{"formatFinding/skip", func(o *bytes.Buffer) {
			fmt.Fprintln(o, plainText(formatFinding(integration.AuditFinding{
				Status: integration.StatusSkip, Subject: "waired phase", Detail: "not applicable"})))
		}},
		{"formatCatalogDetail", func(o *bytes.Buffer) {
			fmt.Fprint(o, plainText(formatCatalogDetail(catalogDetailResp{Engine: "ollama"})))
		}},
	}

	for _, r := range renderers {
		t.Run(r.name, func(t *testing.T) {
			var out bytes.Buffer
			r.run(&out)
			if out.Len() == 0 {
				t.Fatalf("%s wrote nothing; the guard would pass vacuously", r.name)
			}
			if bad, line := firstNonASCII(out.String()); bad != 0 {
				t.Errorf("%s emitted U+%04X on the plain path, in:\n  %s\n"+
					"a CP932 console decodes those UTF-8 bytes as Shift_JIS (#629) — "+
					"add the character to asciiFolder in ascii.go", r.name, bad, line)
			}
		})
	}
}

func isASCII(s string) bool {
	r, _ := firstNonASCII(s)
	return r == 0
}

// firstNonASCII returns the first rune outside ASCII and the line it sits on,
// or 0 when there is none.
func firstNonASCII(s string) (rune, string) {
	for _, line := range strings.Split(s, "\n") {
		for _, r := range line {
			if r > unicode.MaxASCII {
				return r, fmt.Sprintf("%q", line)
			}
		}
	}
	return 0, ""
}

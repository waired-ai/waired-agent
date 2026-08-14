package main

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
)

// unixManagedPath / windowsManagedPath stand in for claudemanaged.Path()
// on each family. planClaudeRoute takes the path as a fact rather than
// reading it, so one host can exercise every OS (CLAUDE.md §Cross-OS
// parity).
const (
	unixManagedPath    = "/etc/claude-code/managed-settings.json"
	windowsManagedPath = `C:\Program Files\ClaudeCode\managed-settings.json`
)

// PRODUCT CONTRACT (#294): `waired init` is the single decider of Claude
// Code routing. The installers deleted their own post-init `waired claude
// enable` — an unconditional enable there overrode an interactive "no" —
// and forward their opt-out as --skip-claude-route instead. Every row
// below is a promise some caller depends on, not a record of today's
// behaviour.
func TestPlanClaudeRoute(t *testing.T) {
	// The baseline: consented, elevated, a real managed-settings path,
	// no opt-out, interactive, no wizard. Each case names only what it
	// changes, so a new field cannot silently alter unrelated rows.
	base := claudeRouteFacts{
		integConsent: true,
		elevated:     true,
		managedPath:  unixManagedPath,
	}
	with := func(mut func(*claudeRouteFacts)) claudeRouteFacts {
		f := base
		mut(&f)
		return f
	}

	cases := []struct {
		name  string
		facts claudeRouteFacts
		want  claudeRouteAction
	}{
		{
			// waired#772: the deferred question. An interactive install
			// asks at the end, once the local stack can actually serve.
			"interactive, everything in place -> ask",
			base, claudeRouteAsk,
		},
		{
			// The installers' --yes path. The consent already given is
			// the answer; there is nobody to prompt.
			"non-interactive -> apply without asking",
			with(func(f *claudeRouteFacts) { f.nonInteractive = true }), claudeRouteApply,
		},
		{
			// waired#835 §4.2: while the browser drives setup the terminal
			// must not ask its own questions. The wizard's claude-code
			// toggle IS the consent.
			"wizard driving -> apply without asking",
			with(func(f *claudeRouteFacts) { f.wizardDriving = true }), claudeRouteApply,
		},
		{
			// The gate the installers' deleted post-init enable bypassed.
			// It must win over everything else.
			"opt-out overrides an interactive run",
			with(func(f *claudeRouteFacts) { f.skipClaudeRoute = true }), claudeRouteNone,
		},
		{
			"opt-out overrides a non-interactive run",
			with(func(f *claudeRouteFacts) {
				f.skipClaudeRoute = true
				f.nonInteractive = true
			}), claudeRouteNone,
		},
		{
			// An opt-out must not produce an elevation hint for work it
			// was told not to do.
			"opt-out outranks the elevation gate",
			with(func(f *claudeRouteFacts) {
				f.skipClaudeRoute = true
				f.elevated = false
			}), claudeRouteNone,
		},
		{
			// Routing is part of the Claude Code integration, not a
			// separate product: declining the integration declines it.
			"no integration consent -> nothing",
			with(func(f *claudeRouteFacts) { f.integConsent = false }), claudeRouteNone,
		},
		{
			// waired#749: a non-elevated run must SAY it cannot route.
			// The silent skip is what left the consent copy looking like
			// routing had been enabled.
			"consented but not elevated -> elevation hint",
			with(func(f *claudeRouteFacts) { f.elevated = false }), claudeRouteNeedsElevation,
		},
		{
			// Windows counts as elevated via elevation.IsElevated(), not
			// a euid check (-1 there); the caller passes the real answer.
			"windows, elevated -> ask",
			with(func(f *claudeRouteFacts) { f.managedPath = windowsManagedPath }), claudeRouteAsk,
		},
		{
			"windows, not elevated -> elevation hint",
			with(func(f *claudeRouteFacts) {
				f.managedPath = windowsManagedPath
				f.elevated = false
			}), claudeRouteNeedsElevation,
		},
		{
			// No managed-settings location at all: nothing to write, and
			// an elevation hint would be a lie — elevation would not help.
			"unsupported OS -> nothing, not an elevation hint",
			with(func(f *claudeRouteFacts) { f.managedPath = "" }), claudeRouteNone,
		},
		{
			"unsupported OS and not elevated -> nothing",
			with(func(f *claudeRouteFacts) {
				f.managedPath = ""
				f.elevated = false
			}), claudeRouteNone,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := planClaudeRoute(c.facts); got != c.want {
				t.Errorf("planClaudeRoute(%+v) = %v, want %v", c.facts, got, c.want)
			}
		})
	}
}

// stubApplyClaudeRoute swaps the write seam for a recorder and returns
// it. The fake takes the REAL options (CLAUDE.md §Test discipline: a fake
// that drops a parameter makes the failing case unwritable) so a test can
// assert WHICH state dir was routed and whether prompting was allowed.
//
// It also performs the write's one observable effect: a managed-settings file
// carrying the base URL for that state dir. Recording the call and dropping the
// effect is the same defect one level up — since waired-agent#796 the closing
// card reads that file to decide what it says about Claude Code, so a fake that
// writes nothing makes "did the card report the routing?" unwritable, which is
// the exact question the issue is about. The path is the one sealed for the
// whole binary in seams_test.go, never the real machine-wide location.
type routeRecorder struct {
	calls []claudeRouteApplyOpts
	err   error
}

func (r *routeRecorder) count() int { return len(r.calls) }

func stubApplyClaudeRoute(t *testing.T, err error) *routeRecorder {
	t.Helper()
	rec := &routeRecorder{err: err}
	prev := applyClaudeRouteFn
	applyClaudeRouteFn = func(o claudeRouteApplyOpts) (string, error) {
		rec.calls = append(rec.calls, o)
		if rec.err != nil {
			return "", rec.err
		}
		path := claudemanaged.Path()
		baseURL, _ := claudeBaseURL(o.StateDir)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("seed managed settings dir: %v", err)
		}
		body := `{"env":{"ANTHROPIC_BASE_URL":` + strconv.Quote(baseURL) + `}}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("seed managed settings: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(path) })
		return path, nil
	}
	t.Cleanup(func() { applyClaudeRouteFn = prev })
	return rec
}

func routingPrompt(t *testing.T, input string, apply func() bool) (bool, string) {
	t.Helper()
	var out bytes.Buffer
	ok := promptClaudeRoutingWith(&out, bufio.NewScanner(strings.NewReader(input)), "http://127.0.0.1:18080", apply)
	return ok, out.String()
}

func TestPromptClaudeRouting_YesApplies(t *testing.T) {
	applied := 0
	ok, out := routingPrompt(t, "y\n", func() bool {
		applied++
		return true
	})
	if !ok || applied != 1 {
		t.Errorf("yes must apply once and report routed: ok=%v applied=%d", ok, applied)
	}
	// The disclosure names the gateway it is about to point Claude at.
	if !strings.Contains(out, "http://127.0.0.1:18080") {
		t.Errorf("expected the target gateway in the disclosure, got:\n%s", out)
	}
}

func TestPromptClaudeRouting_NoSkipsAndHints(t *testing.T) {
	ok, out := routingPrompt(t, "n\n", func() bool {
		t.Error("declining must not write managed settings")
		return false
	})
	if ok {
		t.Error("no must report not-routed")
	}
	for _, want := range []string{
		"Routing left off",
		elevatedCmdline(runtime.GOOS, "waired claude enable"),
		"waired claude route",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("decline output missing %q; got:\n%s", want, out)
		}
	}
}

// EOF takes the default (Yes) — the same convention as every other init
// prompt; real installs reattach stdin to /dev/tty so this only affects
// scripted runs.
func TestPromptClaudeRouting_EOFDefaultsYes(t *testing.T) {
	applied := 0
	ok, _ := routingPrompt(t, "", func() bool {
		applied++
		return true
	})
	if !ok || applied != 1 {
		t.Errorf("EOF must take the Yes default: ok=%v applied=%d", ok, applied)
	}
}

// A failed write must never be reported as routed — the success box reads
// this bool, and "Claude routed through Waired" over a failed write is how
// an operator walks away believing local inference is serving Claude.
func TestRouteClaudeNow_FailureReportsNotRouted(t *testing.T) {
	stubApplyClaudeRoute(t, errors.New("permission denied"))
	var out bytes.Buffer
	if routeClaudeNow(claudeRouteApplyOpts{StateDir: t.TempDir()}, &out) {
		t.Error("a failed managed-settings write must report not-routed")
	}
}

func TestRouteClaudeNow_SuccessReportsRoutedAndTarget(t *testing.T) {
	rec := stubApplyClaudeRoute(t, nil)
	var out bytes.Buffer
	dir := t.TempDir()
	if !routeClaudeNow(claudeRouteApplyOpts{StateDir: dir, AllowPrompt: true}, &out) {
		t.Fatal("a successful write must report routed")
	}
	if rec.count() != 1 {
		t.Fatalf("apply called %d times, want 1", rec.count())
	}
	if rec.calls[0].StateDir != dir {
		t.Errorf("apply got StateDir %q, want %q", rec.calls[0].StateDir, dir)
	}
	if !rec.calls[0].AllowPrompt {
		t.Error("AllowPrompt must reach the apply step")
	}
	// The summary states where Claude Code now points; 9472 is the
	// default ClaudeGatewayPort an empty state dir resolves to.
	if got := out.String(); !strings.Contains(got, "ANTHROPIC_BASE_URL=http://127.0.0.1:9472") {
		t.Errorf("expected the applied-route summary, got:\n%s", got)
	}
}

package main

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

func TestResolveBrowserGate(t *testing.T) {
	cases := []struct {
		name                                            string
		noBrowser, nonInteractive, stdinTTY, hasDisplay bool
		want                                            browserGate
	}{
		{"no-browser wins over everything", true, false, true, true, gatePrintOnly},
		{"no-browser even when non-interactive", true, true, false, true, gatePrintOnly},
		{"headless (no display) prints only", false, false, true, false, gatePrintOnly},
		{"headless non-interactive prints only", false, true, false, false, gatePrintOnly},
		{"non-interactive auto-opens (never hangs)", false, true, true, true, gateAutoOpen},
		{"piped stdin auto-opens (cannot prompt)", false, false, false, true, gateAutoOpen},
		{"interactive TTY with display prompts", false, false, true, true, gatePrompt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveBrowserGate(tc.noBrowser, tc.nonInteractive, tc.stdinTTY, tc.hasDisplay)
			if got != tc.want {
				t.Errorf("resolveBrowserGate(%v,%v,%v,%v) = %v, want %v",
					tc.noBrowser, tc.nonInteractive, tc.stdinTTY, tc.hasDisplay, got, tc.want)
			}
		})
	}
}

// stubOpener swaps openBrowserFn and records calls; restore via t.Cleanup.
func stubOpener(t *testing.T, err error) *int {
	t.Helper()
	calls := 0
	orig := openBrowserFn
	openBrowserFn = func(string) error { calls++; return err }
	t.Cleanup(func() { openBrowserFn = orig })
	return &calls
}

func TestPresentLoginURL_PrintOnly(t *testing.T) {
	calls := stubOpener(t, nil)
	var out strings.Builder
	presentLoginURL(bufio.NewScanner(strings.NewReader("")), &out, "https://cp.example/login/abc", "XKCD-42", gatePrintOnly)
	if *calls != 0 {
		t.Errorf("browser opened %d times in print-only mode", *calls)
	}
	for _, want := range []string{"https://cp.example/login/abc", "XKCD-42"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q: %q", want, out.String())
		}
	}
	// #184: the gate above this one means "Enter opens your browser".
	// This one has nothing for Enter to do, and saying so is what stops
	// the keystroke from being spent on the next thing that reads stdin.
	if !strings.Contains(out.String(), "Nothing to press here") {
		t.Errorf("print-only gate does not say Enter has no role here: %q", out.String())
	}
}

// TestPresentLoginURL_PrintOnlyReadsNothing pins the other half of #184:
// this gate must not consume a line either. Blocking here would strand a
// terminal whose operator signed in on another device, so the keystroke
// is answered by the caller's poll loop instead.
func TestPresentLoginURL_PrintOnlyReadsNothing(t *testing.T) {
	stubOpener(t, nil)
	var out strings.Builder
	sc := bufio.NewScanner(strings.NewReader("typed-ahead\n"))
	presentLoginURL(sc, &out, "https://cp.example/login/abc", "", gatePrintOnly)
	if !sc.Scan() || sc.Text() != "typed-ahead" {
		t.Errorf("the print-only gate consumed the pending line (got %q)", sc.Text())
	}
}

func TestPresentLoginURL_AutoOpen_URLPrintedBeforeOpen(t *testing.T) {
	var out strings.Builder
	orig := openBrowserFn
	var urlAlreadyPrinted bool
	openBrowserFn = func(string) error {
		urlAlreadyPrinted = strings.Contains(out.String(), "https://cp.example/login/abc")
		return nil
	}
	t.Cleanup(func() { openBrowserFn = orig })
	presentLoginURL(bufio.NewScanner(strings.NewReader("")), &out, "https://cp.example/login/abc", "", gateAutoOpen)
	if !urlAlreadyPrinted {
		t.Error("browser opened before the URL was printed")
	}
	if !strings.Contains(out.String(), "Opened your browser") {
		t.Errorf("missing open confirmation: %q", out.String())
	}
}

func TestPresentLoginURL_Prompt_OpensAfterEnter(t *testing.T) {
	calls := stubOpener(t, nil)
	var out strings.Builder
	presentLoginURL(bufio.NewScanner(strings.NewReader("\n")), &out, "https://cp.example/login/abc", "", gatePrompt)
	if *calls != 1 {
		t.Errorf("browser open calls = %d, want 1", *calls)
	}
	if !strings.Contains(out.String(), "Press Enter to open your browser") {
		t.Errorf("missing the Enter prompt: %q", out.String())
	}
}

func TestPresentLoginURL_OpenFailureFallsBackToLink(t *testing.T) {
	calls := stubOpener(t, errors.New("no xdg-open"))
	var out strings.Builder
	presentLoginURL(bufio.NewScanner(strings.NewReader("")), &out, "https://cp.example/login/abc", "", gateAutoOpen)
	if *calls != 1 {
		t.Errorf("browser open calls = %d, want 1", *calls)
	}
	if !strings.Contains(out.String(), "use the link above") {
		t.Errorf("missing the manual-link fallback: %q", out.String())
	}
}

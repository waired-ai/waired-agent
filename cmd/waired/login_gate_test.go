package main

import (
	"errors"
	"strings"
	"testing"
	"time"
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

// waitQueued blocks until the owner's reader goroutine has published n
// lines, so a test that means to exercise Discard (or a poll) is not
// racing the goroutine that produces the line.
func waitQueued(t *testing.T, s *stdinReader, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.lines) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d line(s) to reach the stdin owner", n)
}

// pollGate drives the gate the way the login poll loop does, until want
// browser opens have happened.
func pollGate(t *testing.T, g *loginGate, out *strings.Builder, calls *int, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		g.Poll(out)
		if *calls >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d browser open(s), got %d", want, *calls)
}

func TestPresentLoginURL_PrintOnly(t *testing.T) {
	calls := stubOpener(t, nil)
	var out strings.Builder
	presentLoginURL(nil, &out, "https://cp.example/login/abc", "XKCD-42", "https://cp.example", gatePrintOnly)
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
	if !strings.Contains(out.String(), "Waiting for sign-in to complete") {
		t.Errorf("print-only gate does not say it is waiting: %q", out.String())
	}
}

// TestPresentLoginURL_PrintOnlyReadsNothing pins the other half of #184:
// this gate must not consume a line either. Blocking here would strand a
// terminal whose operator signed in on another device, so the keystroke
// is answered by the caller's poll loop instead.
func TestPresentLoginURL_PrintOnlyReadsNothing(t *testing.T) {
	stubOpener(t, nil)
	var out strings.Builder
	s := newStdinReader(strings.NewReader("typed-ahead\n"))
	waitQueued(t, s, 1)
	presentLoginURL(s, &out, "https://cp.example/login/abc", "", "https://cp.example", gatePrintOnly)
	if line, ok := s.Poll(); !ok || line != "typed-ahead" {
		t.Errorf("the print-only gate consumed the pending line (got %q, ok=%v)", line, ok)
	}
}

// TestLoginGate_PrintOnlyAcksAStrayEnterOnPoll pins that the #184
// acknowledgement moved into the gate with the keystroke it answers: the
// login loop now polls the gate rather than the owner directly.
func TestLoginGate_PrintOnlyAcksAStrayEnterOnPoll(t *testing.T) {
	stubOpener(t, nil)
	var out strings.Builder
	s := newStdinReader(strings.NewReader("\n"))
	g := presentLoginURL(s, &out, "https://cp.example/login/abc", "", "https://cp.example", gatePrintOnly)
	waitQueued(t, s, 1)
	g.Poll(&out)
	if !strings.Contains(out.String(), "Nothing to press here — waiting for you to sign in") {
		t.Errorf("print-only gate did not answer the stray Enter: %q", out.String())
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
	presentLoginURL(nil, &out, "https://cp.example/login/abc", "", "https://cp.example", gateAutoOpen)
	if !urlAlreadyPrinted {
		t.Error("browser opened before the URL was printed")
	}
	if !strings.Contains(out.String(), "Opened your browser") {
		t.Errorf("missing open confirmation: %q", out.String())
	}
}

// TestLoginGate_PromptDoesNotBlock is the #308 product contract, and it
// INVERTS the old TestPresentLoginURL_Prompt_OpensAfterEnter, which pinned
// the blocking read this issue is about: presentLoginURL used to Scan()
// inside the login poll loop, so the CLI stopped polling /login/status —
// and stopped attaching the setup executor — until Enter was pressed.
// Presenting must now return with the browser unopened even when a
// keystroke is already available.
func TestLoginGate_PromptDoesNotBlock(t *testing.T) {
	calls := stubOpener(t, nil)
	var out strings.Builder
	s := newStdinReader(strings.NewReader("\n"))
	waitQueued(t, s, 1)
	g := presentLoginURL(s, &out, "https://cp.example/login/abc", "", "https://cp.example", gatePrompt)
	if *calls != 0 {
		t.Errorf("presenting the prompt gate opened %d browser(s); it must not read stdin", *calls)
	}
	if !strings.Contains(out.String(), "Press Enter to open your browser") {
		t.Errorf("missing the Enter prompt: %q", out.String())
	}
	// The prompt line parks the cursor on purpose: nothing may be said
	// after it until the offer resolves.
	if strings.Contains(out.String(), "Waiting for sign-in to complete") {
		t.Errorf("prompt gate announced the wait before the offer resolved: %q", out.String())
	}
	if !g.armed {
		t.Error("prompt gate is not armed, so a later Enter would do nothing")
	}
}

// TestLoginGate_PromptOpensOnALaterPoll is the other half: Enter still
// opens the browser, just from the loop's poll instead of a blocking read.
func TestLoginGate_PromptOpensOnALaterPoll(t *testing.T) {
	calls := stubOpener(t, nil)
	var out strings.Builder
	s := newStdinReader(strings.NewReader("\n"))
	g := presentLoginURL(s, &out, "https://cp.example/login/abc", "", "https://cp.example", gatePrompt)
	pollGate(t, g, &out, calls, 1)
	if !strings.Contains(out.String(), "Waiting for sign-in to complete") {
		t.Errorf("gate did not announce the wait after opening: %q", out.String())
	}
	// The offer is spent: further keystrokes belong to the takeover offer
	// downstream, not to a second browser window.
	for i := 0; i < 5; i++ {
		g.Poll(&out)
	}
	if *calls != 1 {
		t.Errorf("browser open calls = %d, want 1", *calls)
	}
}

// TestLoginGate_WithdrawnGateNeverOpens covers the case #308 exists for:
// the operator opened the link themselves and signed in without ever
// pressing Enter. The offer is withdrawn, the queued keystroke is dropped
// (#184 — it must not answer the takeover question downstream), and no
// browser is flung at a page that is already done.
func TestLoginGate_WithdrawnGateNeverOpens(t *testing.T) {
	calls := stubOpener(t, nil)
	var out strings.Builder
	s := newStdinReader(strings.NewReader("\n"))
	g := presentLoginURL(s, &out, "https://cp.example/login/abc", "", "https://cp.example", gatePrompt)
	waitQueued(t, s, 1)

	g.Withdraw(&out)
	if len(s.lines) != 0 {
		t.Errorf("withdrawing left %d keystroke(s) queued for the next question", len(s.lines))
	}
	if g.armed {
		t.Error("gate is still armed after being withdrawn")
	}
	for i := 0; i < 5; i++ {
		g.Poll(&out)
	}
	if *calls != 0 {
		t.Errorf("browser opened %d times after the sign-in already completed", *calls)
	}
	// Nothing is said: the phase line the caller prints next ("Signed in
	// — starting Waired on this device...") is what tells the operator
	// the Enter offer is over. Withdraw only closes the parked prompt
	// line so that line does not land on it.
	if !strings.HasSuffix(out.String(), "\n") {
		t.Errorf("withdraw left the prompt line unterminated: %q", out.String())
	}
	// Withdrawing twice must stay silent and idempotent.
	before := out.String()
	g.Withdraw(&out)
	if out.String() != before {
		t.Errorf("second withdraw wrote %q", strings.TrimPrefix(out.String(), before))
	}
}

// TestLoginGate_WithdrawAfterOpeningIsSilent: once Enter has opened the
// browser there is no offer left to withdraw, so a later sign-in must not
// close a line that was already closed.
func TestLoginGate_WithdrawAfterOpeningIsSilent(t *testing.T) {
	calls := stubOpener(t, nil)
	var out strings.Builder
	s := newStdinReader(strings.NewReader("\n"))
	g := presentLoginURL(s, &out, "https://cp.example/login/abc", "", "https://cp.example", gatePrompt)
	pollGate(t, g, &out, calls, 1)
	before := out.String()
	g.Withdraw(&out)
	if out.String() != before {
		t.Errorf("withdraw wrote %q after the gate had already resolved",
			strings.TrimPrefix(out.String(), before))
	}
}

// TestLoginGate_NilAndOwnerlessAreInert pins that every gate method is
// safe where there is no keyboard: off a TTY the owner is nil (main.go),
// and the auto-open gate has nothing to poll for at all.
func TestLoginGate_NilAndOwnerlessAreInert(t *testing.T) {
	calls := stubOpener(t, nil)
	var out strings.Builder
	var nilGate *loginGate
	nilGate.Poll(&out)
	nilGate.Withdraw(&out)

	for _, mode := range []browserGate{gatePrintOnly, gateAutoOpen, gatePrompt} {
		g := presentLoginURL(nil, &out, "https://cp.example/login/abc", "", "https://cp.example", mode)
		g.Poll(&out)
		g.Withdraw(&out)
	}
	// One open, from the auto-open gate at presentation time.
	if *calls != 1 {
		t.Errorf("browser open calls = %d, want 1 (auto-open only)", *calls)
	}
}

func TestPresentLoginURL_OpenFailureFallsBackToLink(t *testing.T) {
	calls := stubOpener(t, errors.New("no xdg-open"))
	var out strings.Builder
	presentLoginURL(nil, &out, "https://cp.example/login/abc", "", "https://cp.example", gateAutoOpen)
	if *calls != 1 {
		t.Errorf("browser open calls = %d, want 1", *calls)
	}
	if !strings.Contains(out.String(), "use the link above") {
		t.Errorf("missing the manual-link fallback: %q", out.String())
	}
}

package main

import (
	"io"

	"github.com/waired-ai/waired-agent/internal/platform/browser"
)

// The sign-in used to fling the browser open the moment the login URL
// existed, with the URL printed only afterwards as a fallback — a first-run
// surprise ("why did a browser just open?"). It now behaves like
// `gcloud auth login`: the URL is ALWAYS printed first, and on an
// interactive terminal the browser opens only after the operator presses
// Enter (or they can just open the link themselves).

// browserGate is how presentLoginURL should treat the browser for one
// concrete session. Factored out so the decision is table-testable.
type browserGate int

const (
	// gatePrompt: print the URL, wait for Enter, then open the browser.
	// The interactive default.
	gatePrompt browserGate = iota
	// gateAutoOpen: print the URL, then open the browser immediately —
	// sessions that cannot answer a prompt (--non-interactive, piped
	// stdin) must never hang on one.
	gateAutoOpen
	// gatePrintOnly: print the URL (+ pairing code) and never touch a
	// browser: --no-browser, or no display to open one on (headless).
	gatePrintOnly
)

// resolveBrowserGate picks the gate from the session's shape. hasDisplay
// comes from internal/platform/browser.HasDisplay (false on a headless
// Linux session, where xdg-open would "succeed" into nothing).
func resolveBrowserGate(noBrowser, nonInteractive, stdinTTY, hasDisplay bool) browserGate {
	if noBrowser || !hasDisplay {
		return gatePrintOnly
	}
	if nonInteractive || !stdinTTY {
		return gateAutoOpen
	}
	return gatePrompt
}

// resolveGateFn is a test seam over resolveBrowserGate: an end-to-end test
// of the login loop runs off a TTY, where the real decision can only ever
// be gateAutoOpen / gatePrintOnly — and the gate #308 is about is the
// third one. The real function keeps its own table test above.
var resolveGateFn = resolveBrowserGate

// openBrowserFn is a test seam over the per-OS browser opener.
var openBrowserFn = browser.Open

// loginGate owns the sign-in step's side of the keyboard for the whole of
// the login poll, and never blocks on it (#308).
//
// This used to be a straight-line function that called Scan() on the
// operator's Enter — from inside runInitViaDaemon's /login/status poll
// loop. An operator who opened the printed link and drove the wizard from
// the browser therefore stopped the CLI dead: no polling, no
// LoginPhaseActive, no setup executor attached, and every wizard step
// failing on "cannot install unprivileged" until Enter was finally
// pressed. The gate is now a state object the loop polls once per tick —
// the shape enterWatch (init_takeover.go) already uses for the takeover
// offer, nil-safe and inert for the same reasons.
type loginGate struct {
	mode browserGate
	// in is the process's stdin owner, or nil where there is no keyboard
	// (off a TTY). A nil owner makes Poll inert: nothing can be typed, so
	// nothing is read.
	in  *stdinReader
	url string
	// armed is the gatePrompt offer still standing: Enter would open the
	// browser. Cleared by the first keystroke and by Withdraw, so the
	// offer resolves exactly once.
	armed bool
}

// presentLoginURL renders the sign-in link according to the gate and
// returns the gate the caller polls for the rest of the login. The URL is
// printed before any browser opens in every mode, so the operator always
// has the link in hand.
//
// in is the caller's stdin owner, never a scanner of our own: a second
// reader layered over stdin here is how an Enter pressed at this step
// ended up answering a later question (#184, #223). It is deliberately the
// concrete owner rather than a lineReader — this step must poll, never
// block.
func presentLoginURL(in *stdinReader, out io.Writer, loginURL, userCode string, mode browserGate) *loginGate {
	g := &loginGate{mode: mode, in: in, url: loginURL}
	writePromptf(out, "\nSign in using this link:\n  %s\n", loginURL)
	switch mode {
	case gatePrintOnly:
		if userCode != "" {
			writePromptf(out, "\nCode: %s\n", userCode)
		}
		writePromptf(out, "\nOpen the link on this or another device.\n")
		// #184: the two gates sit at the same step and used to teach
		// opposite meanings for the same key — Enter opens a browser
		// above, Enter does nothing here. Say so, because the next thing
		// that reads stdin is the browser-setup takeover offer, and an
		// unexplained keystroke landing there is a silent mode switch.
		writePromptf(out, "%s\n", dim("Nothing to press here — sign-in continues on its own once you open the link."))
		g.waiting(out)
	case gatePrompt:
		writePromptf(out, "\n%s Press Enter to open your browser (or open the link above yourself)... ", emo("🌐", ">>"))
		// The prompt parks the cursor with no newline of its own, so
		// nothing more is said until the offer resolves: in Poll, when
		// Enter arrives, or in Withdraw, when the sign-in completes
		// without it.
		g.armed = true
	case gateAutoOpen:
		openLoginURL(out, loginURL)
		g.waiting(out)
	}
	return g
}

// Poll advances the gate with whatever the operator has already typed. It
// never blocks: the link may well be opened on a phone, and a terminal
// parked on a read would never see the sign-in complete.
func (g *loginGate) Poll(out io.Writer) {
	if g == nil || g.in == nil {
		return
	}
	switch g.mode {
	case gatePrompt:
		if !g.armed {
			// The offer is spent. Leave the line queued: from here it
			// belongs to the takeover offer downstream, which discards
			// what predates it (#184) and answers what does not.
			return
		}
		if _, typed := g.in.Poll(); !typed {
			return
		}
		g.armed = false
		// The Enter that got us here echoed its own newline, so the
		// parked prompt line is already closed.
		openLoginURL(out, g.url)
		g.waiting(out)
	case gatePrintOnly:
		// #184: nothing in this mode reads stdin, so an Enter pressed out
		// of muscle memory — the other two gates open a browser with it —
		// used to sit in the buffer until the takeover offer answered it,
		// silently switching setup to the terminal at the moment the user
		// was asking for a browser. Answer it here, where it was pressed.
		if _, typed := g.in.Poll(); typed {
			writePrompt(out, dim("Nothing to press here — waiting for you to sign in with the link above."))
		}
	}
}

// Withdraw retires the Enter offer because the sign-in finished (or
// failed) without it — the operator opened the link themselves, or signed
// in on another device.
//
// It says nothing of its own. The caller prints a phase line immediately
// after ("Signed in — starting Waired on this device...", "Device signed
// in"), and a new line appearing is what tells the operator this terminal
// is no longer waiting on a keystroke. All this does is close the parked
// prompt line so that phase line does not land on it, and drop the
// keystrokes typed at an offer that no longer exists (#184) — needed here
// because the takeover offer's own Discard is skipped on the paths that
// never reach it (an older daemon, --no-browser, --non-interactive).
func (g *loginGate) Withdraw(out io.Writer) {
	if g == nil || !g.armed {
		return
	}
	g.armed = false
	writePromptf(out, "\n")
	g.in.Discard()
}

// waiting closes the presentation with the line that says this terminal is
// now doing nothing but waiting for the control plane.
func (g *loginGate) waiting(out io.Writer) {
	writePromptf(out, "%s Waiting for sign-in to complete…\n", emo("⏳", "..."))
}

func openLoginURL(out io.Writer, loginURL string) {
	if err := openBrowserFn(loginURL); err != nil {
		writePromptf(out, "%s Couldn't open a browser automatically (%v) — use the link above.\n",
			emo("⚠️", "!"), err)
		return
	}
	writePromptf(out, "%s Opened your browser. If nothing appeared, use the link above.\n", emo("🌐", ">>"))
}

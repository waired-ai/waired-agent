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

// openBrowserFn is a test seam over the per-OS browser opener.
var openBrowserFn = browser.Open

// presentLoginURL renders the sign-in link for both login journeys (the
// daemon-mediated login and the standalone enroll) according to the gate.
// The URL is printed before any browser opens in every mode, so the
// operator always has the link in hand. Blocking on Enter in gatePrompt is
// fine: the login session outlives it (same model as gcloud).
//
// in is the caller's line source, never a fresh scanner of our own: on the
// daemon path it is the process's stdin owner, and a second reader layered
// over stdin here is how an Enter pressed at this step ended up answering a
// later question (#184).
func presentLoginURL(in lineReader, out io.Writer, loginURL, userCode string, gate browserGate) {
	writePromptf(out, "\nSign in using this link:\n  %s\n", loginURL)
	switch gate {
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
	case gatePrompt:
		writePromptf(out, "\n%s Press Enter to open your browser (or open the link above yourself)... ", emo("🌐", ">>"))
		in.Scan()
		openLoginURL(out, loginURL)
	case gateAutoOpen:
		openLoginURL(out, loginURL)
	}
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

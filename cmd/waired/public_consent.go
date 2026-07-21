package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// errPublicUseUnsupported is returned when the daemon answers the
// consumer Public Share routes with 404 — an older waired-agent that
// predates the /waired/v1/public/{use,warning,consent} handlers. The
// consumer verbs have nothing to fall back to, so they surface this as a
// hard error telling the operator to upgrade.
var errPublicUseUnsupported = errors.New("daemon does not expose public share settings; upgrade waired-agent")

// errPublicConsentDeclined signals that the user saw the first-use
// warning and answered No. Callers translate it into a calm, zero-exit
// "nothing changed" outcome — a decline is a valid choice, not a failure.
var errPublicConsentDeclined = errors.New("public share: not enabled")

// stdinIsInteractiveFn is a seam over stdinIsInteractive so tests can
// force the interactive branch. stdinIsInteractive reads the real
// os.Stdin, which is never a character device under `go test`, so the
// consent prompt would otherwise refuse in every test.
var stdinIsInteractiveFn = stdinIsInteractive

// fetchPublicWarning reads the first-use warning that the daemon serves —
// the single source of the consent copy for every UI surface. A 404
// means the daemon predates the consumer routes, mapped to the
// upgrade-me sentinel.
func fetchPublicWarning(mgmt string) (management.PublicWarningResponse, error) {
	var w management.PublicWarningResponse
	if err := publicGetJSON(mgmt, "/waired/v1/public/warning", &w); err != nil {
		if isMgmtStatus(err, http.StatusNotFound) {
			return management.PublicWarningResponse{}, errPublicUseUnsupported
		}
		return management.PublicWarningResponse{}, err
	}
	return w, nil
}

// acceptPublicConsent records the user's acceptance of a specific warning
// version. The daemon rejects a version that no longer matches the served
// text (409 warning_version_mismatch), which the caller handles.
func acceptPublicConsent(mgmt string, warningVersion int) (management.PublicUseResponse, error) {
	var r management.PublicUseResponse
	err := publicPostJSON(mgmt, "/waired/v1/public/consent",
		management.PublicConsentRequest{WarningVersion: warningVersion}, &r)
	return r, err
}

// ensurePublicConsent guarantees that this computer has accepted the
// current first-use privacy warning before any public-use setting is
// applied (spec §4.2 + §14). When consent already exists it returns
// immediately with no prompt. Otherwise it shows the server-authored
// warning VERBATIM, asks for an interactive y/N (default No), records the
// accepted version, and — user-approved single accept = both sides on —
// turns on sharing of this computer too.
//
// The whole warning text is DATA supplied by the daemon: not one word of
// consent copy is hardcoded here, so a single source drives the CLI and
// the Tray dialog alike.
func ensurePublicConsent(mgmt string, out io.Writer, in io.Reader) (management.PublicUseResponse, error) {
	// 1. Already consented? Do nothing (no prompt, no reciprocity).
	var cur management.PublicUseResponse
	if err := publicGetJSON(mgmt, "/waired/v1/public/use", &cur); err != nil {
		if isMgmtStatus(err, http.StatusNotFound) {
			return management.PublicUseResponse{}, errPublicUseUnsupported
		}
		return management.PublicUseResponse{}, err
	}
	if cur.Consented {
		return cur, nil
	}

	// A single scanner across both possible prompt rounds: bufio.Scanner
	// buffers ahead, so a fresh scanner per round on the same reader would
	// drop the input the first one already consumed (breaks the
	// re-fetch-after-mismatch round).
	sc := bufio.NewScanner(in)

	// 2–5. Display the warning, prompt, record consent. A
	// warning_version_mismatch (the served text changed between our GET
	// /warning and our POST /consent) triggers exactly one re-fetch; a
	// second mismatch is a hard error.
	var resp management.PublicUseResponse
	for attempt := 0; ; attempt++ {
		w, err := fetchPublicWarning(mgmt)
		if err != nil {
			return management.PublicUseResponse{}, err
		}

		// Server-authored copy, printed VERBATIM: title, then the body —
		// including its trailing "More: …" line, which is part of w.Text.
		writePrompt(out, w.Title)
		writePrompt(out, w.Text)

		// Non-interactive guard. --yes MUST NOT reach here (this flow
		// takes no such flag): recording consent for text the user never
		// saw would defeat the whole point of warning-version pinning.
		if !stdinIsInteractiveFn() {
			return management.PublicUseResponse{}, fmt.Errorf(
				"waired public: this is your first time using public nodes — " +
					"run it in a terminal to read and accept the privacy warning")
		}

		if !ynPrompt(out, sc, w.AcceptLabel, false) {
			// Echo the server's cancel wording so the decline reads in the
			// same voice as the warning.
			writePrompt(out, w.CancelLabel+" — nothing was enabled.")
			return management.PublicUseResponse{}, errPublicConsentDeclined
		}

		resp, err = acceptPublicConsent(mgmt, w.Version)
		if err == nil {
			break
		}
		if mgmtErrorCode(err) == "warning_version_mismatch" && attempt == 0 {
			// The served text changed under us — loop once to re-fetch and
			// re-display before recording consent for the new version.
			continue
		}
		return management.PublicUseResponse{}, err
	}

	// 6. Reciprocity: the single accept above also turns on sharing of
	// this computer (the daemon set the consumer mode/main/sub on first
	// consent — do NOT re-POST those here). Best-effort: consent is
	// already durably recorded, so a sharing hiccup must not fail.
	enableReciprocalShare(mgmt, out)
	return resp, nil
}

// enableReciprocalShare turns on public sharing of this computer after a
// first accept. It never fails the caller: a 404 (daemon without the
// provider routes) is skipped silently, and any other error prints a
// plain-English note but leaves the recorded consent intact.
func enableReciprocalShare(mgmt string, out io.Writer) {
	var share management.PublicShareStateResponse
	if err := publicGetJSON(mgmt, "/waired/v1/public/share", &share); err != nil {
		// 404 → provider routes absent; other errors → best-effort skip.
		// Either way there is nothing safe to reconcile.
		return
	}
	if share.State == string(state.PublicShareOn) {
		return
	}
	var enabled management.PublicShareStateResponse
	if err := publicPostJSON(mgmt, "/waired/v1/public/share/enable", nil, &enabled); err != nil {
		pln(out, "Your consent was saved, but sharing this computer could not be turned on automatically. Run: waired public share")
		return
	}
	// Print the server-authored side-effect note (mesh auto-enable /
	// pending-sync wording) VERBATIM, never re-authored here.
	if enabled.Note != "" {
		pln(out, enabled.Note)
	}
}

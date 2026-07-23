//go:build linux

package tray

import (
	"errors"
	"os/exec"
)

// ConfirmYesNo spawns a desktop confirmation dialog with the given
// title + body and returns the user's choice. The trailing `ok` flag
// reports whether the dialog could even be displayed: when both zenity
// and kdialog are absent (containers, headless test machines, some
// minimal desktop installs) we cannot ask, so callers fall back to a
// safer flow (typically: copy the equivalent shell command to the
// clipboard).
//
// Detection order: `zenity --question` (GNOME / generic) first, then
// `kdialog --yesno` (KDE). Both share an identical binary contract:
// exit 0 = yes, exit 1 = no, anything else = error. We treat error
// the same as `ok=false` so a zenity that's installed but failing
// (e.g. no DISPLAY) still falls through to the clipboard path rather
// than blocking the user.
//
// The function blocks on the spawned process. Callers must invoke
// from a goroutine — the systray click loop must not stall waiting on
// a dialog the user might leave focused for minutes.
func ConfirmYesNo(title, body string) (yes, ok bool) {
	for _, prog := range confirmCandidates(title, body) {
		path, err := exec.LookPath(prog.binary)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, prog.args...) //nolint:gosec // args are static, computed by us
		err = cmd.Run()
		if err == nil {
			return true, true
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Yes/no dialogs return exit 1 for "no". Treat any other
			// non-zero code as "the dialog itself failed" → ok=false
			// so the caller can fall back rather than silently
			// accepting nothing.
			if exitErr.ExitCode() == 1 {
				return false, true
			}
		}
		// Either Run() failed before the process started or the dialog
		// returned a code beyond {0,1} (zenity uses -1 / 5 for cancel).
		// Try the next candidate; only fall through to ok=false when
		// all candidates have been tried.
	}
	return false, false
}

// ConfirmWithLabels is ConfirmYesNo with caller-supplied button labels:
// the affirmative button reads acceptLabel and the negative button
// cancelLabel, carried verbatim to the desktop backend. Used by the
// public-use consent flow (waired#833), whose accept/cancel wording is
// authored server-side and served over the management API so every UI
// surface renders identical text.
//
// Same backend contract and fallback semantics as ConfirmYesNo: zenity
// first, then kdialog; (false,false) when neither is on PATH so callers
// fall back to a non-interactive route.
func ConfirmWithLabels(title, body, acceptLabel, cancelLabel string) (confirmed, ok bool) {
	for _, prog := range confirmLabelCandidates(title, body, acceptLabel, cancelLabel) {
		path, err := exec.LookPath(prog.binary)
		if err != nil {
			continue
		}
		cmd := exec.Command(path, prog.args...) //nolint:gosec // args are static, computed by us
		err = cmd.Run()
		if err == nil {
			return true, true
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Exit 1 = the negative button (cancelLabel). Any other
			// non-zero code means the dialog itself failed → try the next
			// candidate, only reporting ok=false once all are exhausted.
			if exitErr.ExitCode() == 1 {
				return false, true
			}
		}
	}
	return false, false
}

type confirmProgram struct {
	binary string
	args   []string
}

// confirmLabelCandidates mirrors confirmCandidates but threads the
// caller's button labels through: zenity via --ok-label/--cancel-label,
// kdialog via --yes-label/--no-label (which must precede --yesno).
//
// The consent flow is this helper's only caller, and it is the single
// gate between "nothing shared" and "strangers may use this machine",
// so the negative button is the default (waired#901 L5) — the same
// safe-default reasoning ConfirmYesNo documents. kdialog's --yesno
// offers no default-button switch, so on that backend the default stays
// whatever KDE picks; zenity gets --default-cancel.
func confirmLabelCandidates(title, body, acceptLabel, cancelLabel string) []confirmProgram {
	return []confirmProgram{
		{
			binary: "zenity",
			args: []string{
				"--question",
				"--title=" + title,
				"--text=" + body,
				"--ok-label=" + acceptLabel,
				"--cancel-label=" + cancelLabel,
				"--default-cancel",
			},
		},
		{
			binary: "kdialog",
			args: []string{
				"--title", title,
				"--yes-label", acceptLabel,
				"--no-label", cancelLabel,
				"--yesno", body,
			},
		},
	}
}

// confirmCandidates returns the desktop dialog spawns to try, in the
// order ConfirmYesNo prefers them. Pulled out so the unit test can
// stub the binary lookup without forking real GTK / Qt processes.
func confirmCandidates(title, body string) []confirmProgram {
	return []confirmProgram{
		{
			binary: "zenity",
			args: []string{
				"--question",
				"--title=" + title,
				"--text=" + body,
			},
		},
		{
			binary: "kdialog",
			args: []string{
				"--title", title,
				"--yesno", body,
			},
		},
	}
}

// (Earlier revisions defined notify(summary, body) here; it has been
// folded into internal/platform/notification, and tray.go now has a
// notify() helper that wraps notification.New() for the cross-
// platform callers. This stub stays only so a grep finds the trail.)

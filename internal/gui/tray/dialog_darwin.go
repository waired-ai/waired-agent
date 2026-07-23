//go:build darwin

package tray

// ConfirmYesNo shows a yes/no question via osascript and returns the
// user's choice. ok is false only if the dialog system itself was
// unavailable (osascript missing — should never happen on a normal
// macOS install, but defensive). On any failure callers fall through
// to the same "copy command to clipboard" safety path that the Linux
// and Windows backends use.
//
// The dialog is built with the affirmative button second and the
// "Cancel" button first so the default key (Esc / Cmd-.) maps to
// cancel — matches the Windows MessageBox(YESNO) behaviour where
// "No" is the safe default for destructive operations.
func ConfirmYesNo(title, body string) (yes, ok bool) {
	pressed, dialogOk := runOsascriptDialogReturning(
		title, body, "caution",
		[]string{"Cancel", "Yes"},
		"Cancel",
	)
	if !dialogOk {
		return false, false
	}
	return pressed == "Yes", true
}

// ConfirmWithLabels is ConfirmYesNo with caller-supplied button labels.
// The public-use consent flow (waired#833) authors its accept/cancel
// wording server-side and serves it over the management API so every UI
// surface renders identical text; here those strings become the dialog
// buttons.
//
// The affirmative button is placed second so the layout matches
// ConfirmYesNo, and the NEGATIVE button is the default (waired#901 L5):
// this helper's only caller is the consent flow, the single gate
// between "nothing shared" and "strangers may use this machine", and
// the dialog steals focus — a stray Return or Space must not accept it.
// Same safe-default reasoning ConfirmYesNo documents above.
// runOsascriptDialogReturning returns the pressed button string, so we
// compare it to acceptLabel rather than returning its (string,bool)
// directly.
func ConfirmWithLabels(title, body, acceptLabel, cancelLabel string) (confirmed, ok bool) {
	pressed, dialogOk := runOsascriptDialogReturning(
		title, body, "caution",
		[]string{cancelLabel, acceptLabel},
		cancelLabel,
	)
	if !dialogOk {
		return false, false
	}
	return pressed == acceptLabel, true
}

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

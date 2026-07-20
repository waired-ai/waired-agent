//go:build windows

package tray

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// MessageBox return codes (winuser.h).
const (
	idOk     = 1
	idCancel = 2
	idYes    = 6
	idNo     = 7
)

// MessageBox style flags (winuser.h). Only the subset the tray uses.
const (
	mbOk            = 0x00000000
	mbYesNo         = 0x00000004
	mbIconError     = 0x00000010
	mbIconQuestion  = 0x00000020
	mbIconInfo      = 0x00000040
	mbSetForeground = 0x00010000
	mbSystemModal   = 0x00001000
	mbTopMost       = 0x00040000
)

var (
	// shell32 is used by actions_windows.go (ShellExecuteW) to open URLs /
	// files via the user's default handler.
	shell32         = windows.NewLazySystemDLL("shell32.dll")
	procMessageBoxW = user32.NewProc("MessageBoxW")
)

// messageBoxW wraps MessageBoxW. hwnd=0 makes it a top-level dialog
// owned by the desktop, which is what we want for tray-driven
// confirmations.
func messageBoxW(title, body string, flags uintptr) int {
	tptr, _ := windows.UTF16PtrFromString(title)
	bptr, _ := windows.UTF16PtrFromString(body)
	r, _, _ := procMessageBoxW.Call(
		0,
		uintptr(unsafe.Pointer(bptr)),
		uintptr(unsafe.Pointer(tptr)),
		flags|mbSetForeground|mbTopMost,
	)
	return int(r)
}

// ConfirmYesNo shows a Yes/No question. ok is false only if the
// dialog system itself was unavailable; on Windows the MessageBox is
// part of the OS and always renders, so ok is always true unless
// MessageBoxW returns a hard failure (return 0).
func ConfirmYesNo(title, body string) (yes, ok bool) {
	r := messageBoxW(title, body, mbYesNo|mbIconQuestion)
	if r == 0 {
		return false, false
	}
	return r == idYes, true
}

// ConfirmWithLabels is ConfirmYesNo with caller-supplied button labels.
// The public-use consent flow (waired#833) authors its accept/cancel
// wording server-side and serves it over the management API so every UI
// surface renders identical text.
//
// LIMITATION: MessageBoxW only offers a fixed set of button captions
// (Yes/No here) — Windows provides no way to relabel them without a
// full custom dialog/TaskDialog. So instead of rendering the served
// labels as buttons we append them to the BODY text as a legend, and
// map the standard Yes button to confirmed. If the tray ever needs true
// custom captions on Windows, switch this to TaskDialogIndirect.
func ConfirmWithLabels(title, body, acceptLabel, cancelLabel string) (confirmed, ok bool) {
	body = body + "\n\n[Yes = " + acceptLabel + "]  [No = " + cancelLabel + "]"
	r := messageBoxW(title, body, mbYesNo|mbIconQuestion)
	if r == 0 {
		return false, false
	}
	return r == idYes, true
}

func ShowAbout(version, sha string) {
	body := fmt.Sprintf("Waired %s\nbuild %s\n\nhttps://github.com/waired-ai/waired", version, sha)
	messageBoxW("About Waired", body, mbOk|mbIconInfo)
}

func ShowError(message string) {
	messageBoxW("Waired", message, mbOk|mbIconError)
}

func ShowConfirm(prompt string) bool {
	r := messageBoxW("Waired", prompt, mbYesNo|mbIconQuestion)
	return r == idYes
}

//go:build windows

package console

import "golang.org/x/sys/windows"

// The console output code page seams. Swapped in the unit test so the
// behaviour around cpPlan — set on the way in, restore on the way out, do
// nothing when there is no console — is exercised without a real console.
var (
	getOutputCP = windows.GetConsoleOutputCP
	setOutputCP = windows.SetConsoleOutputCP
)

// SetOutputUTF8 switches the console output code page to CP_UTF8 so the UTF-8
// bytes this binary writes are decoded as UTF-8 rather than as the machine's
// ANSI code page (CP932 on a Japanese install — #629). It returns the function
// that puts the previous page back; that function is safe to call even when
// nothing was changed.
//
// Best-effort throughout: a failure leaves the console on its original page,
// OutputIsUTF8 then reports false, and the caller degrades to ASCII instead of
// writing bytes that will be mojibake.
func SetOutputUTF8() (restore func()) {
	cur, err := getOutputCP()
	prior, change := cpPlan(cur, err)
	if !change {
		return func() {}
	}
	if err := setOutputCP(CPUTF8); err != nil {
		return func() {}
	}
	return func() { _ = setOutputCP(prior) }
}

// OutputIsUTF8 reports whether the console this process writes to decodes
// output as UTF-8. False when there is no console at all, which is the right
// answer for a glyph decision: no console means the sink is a pipe or a file,
// and the ASCII path keeps those logs greppable.
func OutputIsUTF8() bool {
	cp, err := getOutputCP()
	return err == nil && cp == CPUTF8
}

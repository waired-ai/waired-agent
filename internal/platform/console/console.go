// Package console makes a program's UTF-8 output survive the terminal it is
// written to.
//
// Windows decodes what a console program writes using the console output code
// page, not UTF-8. On a Japanese install that page is 932, so every UTF-8 byte
// sequence the waired binaries wrote came back as mojibake — U+2014 (—)
// arrived as 窶・and the whole `waired init` transcript was affected (#629).
//
// install.ps1 has always sidestepped this: `[Console]::OutputEncoding =
// [Text.Encoding]::UTF8` flips the console output code page to CP_UTF8, which
// is why the installer banner rendered correctly on the very host whose
// waired.exe output did not. The Go binaries never did the equivalent.
//
// Linux and macOS have no console code page; both entry points are inert there
// and callers fall back to the locale variables.
package console

// CPUTF8 is Windows' CP_UTF8 code page identifier.
const CPUTF8 = 65001

// cpPlan decides what to do about the console output code page, given the page
// currently in force and the error from reading it. Untagged and pure so it is
// table-tested on every platform, not only when CI happens to run a Windows
// job (CLAUDE.md §Cross-OS parity).
//
// change=false means leave the console alone: either there is no console to
// talk to (GetConsoleOutputCP fails for a service, and for a process started
// without one — waired-agent runs under the SCM, waired-tray has no console),
// or the page is already CP_UTF8 and setting it again would only manufacture a
// restore we do not owe.
//
// When change is true the caller sets CP_UTF8 and must put prior back on the
// way out: the console output code page belongs to the console, not to this
// process, so a page left on CP_UTF8 outlives the program and would change how
// every later command in that window renders.
func cpPlan(cur uint32, err error) (prior uint32, change bool) {
	if err != nil {
		return 0, false
	}
	if cur == CPUTF8 {
		return cur, false
	}
	return cur, true
}

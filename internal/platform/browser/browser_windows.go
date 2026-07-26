//go:build windows

package browser

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Open launches the URL with the user's default handler. rundll32
// url.dll,FileProtocolHandler is the safe form: no COM init required, the same
// call `start <url>` makes internally. We spawn via CreateProcess with
// CREATE_NO_WINDOW so there is no console flash, and deliberately do NOT shell
// out via `cmd /c start` (which would inherit stdin).
//
// rundll32 is addressed by its ABSOLUTE path (#181). CreateProcess does not
// search %PATH% for a non-NULL lpApplicationName — it resolves the name
// against the current directory — so the bare "rundll32.exe" this used to pass
// failed with err=2 from the CLI's usual working directory and nothing opened.
//
// There is no privilege de-escalation here, unlike the Unix implementations:
// the CLI's Windows elevation path re-launches through UAC in the same
// interactive session, and HKCU associations resolve normally. What does not
// survive that boundary is the environment block and the argument quoting —
// tracked separately as #192 / #177.
func Open(url string) error {
	if err := validateURL(url); err != nil {
		return err
	}
	app, cmdline := windowsRundllCmd(systemDirectory(), url)

	// A nil lpApplicationName means "resolve from the command line", which
	// restores the pre-regression %PATH% search. Only used when the system
	// directory itself cannot be resolved.
	var appPtr *uint16
	if app != "" {
		p, err := windows.UTF16PtrFromString(app)
		if err != nil {
			return err
		}
		appPtr = p
	}
	args, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return err
	}
	var startupInfo windows.StartupInfo
	startupInfo.Cb = uint32(unsafe.Sizeof(startupInfo))
	var procInfo windows.ProcessInformation
	if err := windows.CreateProcess(
		appPtr, args, nil, nil, false,
		windows.CREATE_NO_WINDOW,
		nil, nil,
		&startupInfo, &procInfo,
	); err != nil {
		return fmt.Errorf("browser.Open: CreateProcess %s: %w", appDescription(app), err)
	}
	_ = windows.CloseHandle(procInfo.Process)
	_ = windows.CloseHandle(procInfo.Thread)
	return nil
}

// systemDirectory returns %SystemRoot%\System32, or "" when it cannot be
// resolved (which is not survivable for anything else either, but Open then
// degrades to the command-line search rather than failing outright).
func systemDirectory() string {
	dir, err := windows.GetSystemDirectory()
	if err != nil {
		return ""
	}
	return dir
}

// appDescription labels the launch target in errors, so a future failure says
// which of the two resolution paths was in play.
func appDescription(app string) string {
	if app == "" {
		return "rundll32.exe (via the command line)"
	}
	return app
}

// HasDisplay reports whether a graphical session is present. On Windows the
// desktop is assumed available (the tray runs in the interactive session, and
// the CLI prints the URL as a fallback if the launch fails).
func HasDisplay() bool { return true }

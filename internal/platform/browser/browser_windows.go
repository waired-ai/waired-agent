//go:build windows

package browser

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Open launches the URL with the user's default handler. rundll32
// url.dll,FileProtocolHandler is the safe form: no COM init required, the same
// call `start <url>` makes internally. We spawn via CreateProcess with
// CREATE_NO_WINDOW so there is no console flash, and deliberately do NOT shell
// out via `cmd /c start` (which would inherit stdin).
func Open(url string) error {
	if url == "" {
		return errors.New("browser.Open: empty url")
	}
	rundll, err := windows.UTF16PtrFromString("rundll32.exe")
	if err != nil {
		return err
	}
	args, err := windows.UTF16PtrFromString(`rundll32.exe url.dll,FileProtocolHandler ` + url)
	if err != nil {
		return err
	}
	var startupInfo windows.StartupInfo
	startupInfo.Cb = uint32(unsafe.Sizeof(startupInfo))
	var procInfo windows.ProcessInformation
	if err := windows.CreateProcess(
		rundll, args, nil, nil, false,
		windows.CREATE_NO_WINDOW,
		nil, nil,
		&startupInfo, &procInfo,
	); err != nil {
		return fmt.Errorf("browser.Open: CreateProcess: %w", err)
	}
	_ = windows.CloseHandle(procInfo.Process)
	_ = windows.CloseHandle(procInfo.Thread)
	return nil
}

// HasDisplay reports whether a graphical session is present. On Windows the
// desktop is assumed available (the tray runs in the interactive session, and
// the CLI prints the URL as a fallback if the launch fails).
func HasDisplay() bool { return true }

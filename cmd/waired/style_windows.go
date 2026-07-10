//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableVTProcessing turns on ENABLE_VIRTUAL_TERMINAL_PROCESSING for stdout so
// classic Windows conhost renders ANSI SGR sequences instead of printing the
// raw escapes. Best-effort: Windows Terminal / PowerShell 7 already enable VT,
// and computeUseColor has already confirmed stdout is a TTY, so a failure here
// is harmless (color simply stays off if the console genuinely can't do VT).
func enableVTProcessing() {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return
	}
	_ = windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING)
}

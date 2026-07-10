//go:build windows

package main

// platformDefaultControlURL: on Windows the installer sets a machine-level
// WAIRED_CONTROL_URL environment variable, which os.Getenv (the --control
// flag default in runInit) already sees for an elevated process. There is
// no separate file to read, so this returns "".
func platformDefaultControlURL() string { return "" }

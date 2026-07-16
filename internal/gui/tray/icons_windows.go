//go:build windows

package tray

import _ "embed"

// Windows fyne.io/systray.SetIcon requires raw .ico bytes; .png is
// accepted on Linux/Darwin but not Win32. The .ico files here are
// generated from the same source PNGs by gen_test.go (run with
// WAIRED_TRAY_REGEN=1 inside internal/gui/tray/icons/).

//go:embed icons/waired-connected.ico
var iconConnected []byte

//go:embed icons/waired-disconnected.ico
var iconDisconnected []byte

//go:embed icons/waired-error.ico
var iconErrorIcon []byte

//go:embed icons/waired-degraded.ico
var iconDegraded []byte

//go:embed icons/waired-busy.ico
var iconBusy []byte

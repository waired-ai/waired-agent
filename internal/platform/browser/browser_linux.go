//go:build linux

package browser

import (
	"os"
	"os/exec"
)

// Open launches the user's preferred handler for url via xdg-open — the only
// standard on Linux desktops, honored by both X11 and Wayland sessions (it is
// a thin shell over .desktop MIME resolution).
func Open(url string) error {
	cmd := exec.Command("xdg-open", url)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

// HasDisplay reports whether a graphical session is present. On Linux a
// headless SSH server has neither DISPLAY (X11) nor WAYLAND_DISPLAY, so
// auto-opening a browser there is pointless — callers print the URL instead.
func HasDisplay() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

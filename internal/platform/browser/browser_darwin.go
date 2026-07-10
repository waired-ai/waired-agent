//go:build darwin

package browser

import (
	"errors"
	"os"
	"os/exec"
)

// Open launches url with the user's default handler. macOS ships `open(1)`,
// the canonical LaunchServices entry point (equivalent to xdg-open on Linux).
func Open(url string) error {
	if url == "" {
		return errors.New("browser.Open: empty url")
	}
	cmd := exec.Command("/usr/bin/open", url)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Start()
}

// HasDisplay reports whether a graphical session is present. On macOS we
// assume the GUI is available (the tray only runs in an Aqua session, and the
// CLI falls back to printing the URL if `open` fails over a headless SSH login).
func HasDisplay() bool { return true }

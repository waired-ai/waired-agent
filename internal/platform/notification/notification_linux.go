//go:build linux

package notification

import (
	"errors"
	"os/exec"
)

// linuxNotifier shells out to notify-send (libnotify). The binary is
// part of standard GNOME / KDE installs; on a stripped environment
// (containers, minimal WMs) we silently no-op rather than logging,
// since the tray cannot recover from a missing notifier.
type linuxNotifier struct{}

func newNotifier() Notifier { return linuxNotifier{} }

func (linuxNotifier) Notify(title, body string, level Level) error {
	if title == "" {
		return errors.New("notification: empty title")
	}
	path, err := exec.LookPath("notify-send")
	if err != nil {
		// no-op silently: minimal desktops simply don't get toasts
		return nil
	}
	args := []string{"--app-name=Waired"}
	switch level {
	case Warning:
		args = append(args, "--urgency=normal", "--icon=dialog-warning")
	case Error:
		args = append(args, "--urgency=critical", "--icon=dialog-error")
	default:
		args = append(args, "--urgency=low", "--icon=dialog-information")
	}
	args = append(args, title)
	if body != "" {
		args = append(args, body)
	}
	// Best-effort: ignore exit status. notify-send returning non-zero
	// usually means the notification daemon is absent (e.g. SSH
	// session, headless boot) — same outcome as binary missing.
	_ = exec.Command(path, args...).Run()
	return nil
}

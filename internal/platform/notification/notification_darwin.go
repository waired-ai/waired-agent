//go:build darwin

package notification

import (
	"errors"
	"os/exec"
	"strings"
)

// darwinNotifier shells out to osascript's `display notification`
// which routes through the macOS Notification Center. We deliberately
// do NOT take the UserNotifications.framework / CGO route — that
// requires a code-signed bundled .app (the framework refuses calls
// from unsigned ad-hoc binaries with errSecMissingEntitlement), which
// is out of scope until installer phase. osascript works from any
// binary because Script Editor / osascript itself owns the
// notification entitlement.
//
// Limitations callers should know about:
//   - macOS suppresses notifications when the app is foregrounded.
//     Toast still renders in Notification Center, just not as a
//     transient banner.
//   - Level is collapsed to a single style — `display notification`
//     does not expose urgency/icon variants for unsigned scripts.
//   - The system may rate-limit when many notifications fire in a
//     short window. The tray's call sites (state transitions, opencode
//     reconfigure) fire at human cadence so we never hit the limit.
type darwinNotifier struct{}

func newNotifier() Notifier { return darwinNotifier{} }

func (darwinNotifier) Notify(title, body string, _ Level) error {
	if title == "" {
		return errors.New("notification: empty title")
	}
	// Build the AppleScript snippet. Both title and body are
	// user-controlled strings that may contain quotes / backslashes;
	// escape both via quoteAppleScript so a body of `key="X"\nfoo`
	// renders correctly.
	script := `display notification ` + quoteAppleScript(body) +
		` with title ` + quoteAppleScript(title)
	// Best-effort: a missing osascript (unimaginable on a real Mac,
	// but possible in a fully stripped CI container) returns nil so
	// the tray does not surface an error for an advisory toast.
	cmd := exec.Command("/usr/bin/osascript", "-e", script)
	_ = cmd.Run()
	return nil
}

// quoteAppleScript wraps s in double quotes with embedded backslashes
// and quotes escaped. Mirrors the helper in
// internal/gui/tray/actions_darwin.go (kept duplicate so this package
// stays dep-free).
func quoteAppleScript(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

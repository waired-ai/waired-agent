package tray

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Reconfiguring a coding-agent integration means running `waired link
// <target>` — the same command the clipboard fallback offers — as THIS
// process, which is the desktop user.
//
// It used to be a POST to the daemon, which then wrote the files itself.
// That made the daemon write into a home directory on behalf of any local
// caller, the privilege bridge waired#935 keeps it out of, and under a
// system-service install it wrote into the service account's home where
// no coding agent ever looks (waired-agent#986). The tray already runs as
// the right user; shelling out to the CLI keeps one implementation of
// what "link" means.
//
// No elevation: `waired link` writes files owned by the user it runs as,
// and running it elevated would recreate the ownership bug from the other
// side. --force re-applies even when the agent binary is not installed
// (the plugin is inert until it is), and --no-prompt is required because
// there is no terminal to ask in.
func wairedLinkArgs(target string) []string {
	return []string{"link", "--force", "--no-prompt", target}
}

// runWairedLink executes the located CLI and folds its output into the
// error, so the tray's notification carries the CLI's own words rather
// than "exit status 1".
func runWairedLink(ctx context.Context, bin, target string) error {
	cmd := exec.CommandContext(ctx, bin, wairedLinkArgs(target)...)
	// No terminal to inherit: a child that reads stdin would block the
	// click forever.
	cmd.Stdin = nil
	out, err := cmd.CombinedOutput()
	if err != nil {
		if detail := lastMeaningfulLine(string(out)); detail != "" {
			return fmt.Errorf("%w: %s", err, detail)
		}
		return err
	}
	return nil
}

// lastMeaningfulLine returns the final non-blank line of the CLI output —
// where a cobra/CLI error lands — clamped so a notification stays a
// notification.
func lastMeaningfulLine(out string) string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			if len(s) > 200 {
				return s[:200]
			}
			return s
		}
	}
	return ""
}

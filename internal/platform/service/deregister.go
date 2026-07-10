package service

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/waired-ai/waired-agent/internal/deauth"
	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// deregisterOnUninstall best-effort revokes this device from the Control
// Plane before the service unit is torn down, so an uninstalled machine
// stops lingering in the admin device list (and in peers' network maps).
// It is the uninstall counterpart to `waired logout`'s deauth step and is
// invoked from the `uninstall` dispatch — which is how the Inno Setup
// uninstaller (Windows), the documented macOS uninstall, and any Linux use
// of `waired-agent uninstall` all reach it. (The Debian package deregisters
// separately, from its prerm, via `waired logout --server-only --revoke`.)
//
// It is deliberately non-fatal: any failure (not enrolled, no token, CP
// unreachable) is warned about and uninstall proceeds regardless. It never
// touches local files — the platform uninstaller / package manager owns
// local state deletion.
//
// The agent's state dir differs by platform and install mode (system-wide
// vs per-user) and can be relocated via $WAIRED_STATE_DIR, so we try each
// candidate and revoke from the first that carries real credentials.
func deregisterOnUninstall() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	seen := map[string]bool{}
	for _, dir := range []string{
		paths.StateDir(paths.System),      // Linux /var/lib/waired, Windows %ProgramData%\waired
		paths.StateDir(paths.Interactive), // macOS ~/Library/Application Support/waired
		paths.StateDir(paths.AutoDetect),
	} {
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true

		outcome, err := deauth.Deregister(ctx, dir, deauth.ModeRevoke)
		if err != nil {
			fmt.Fprintf(os.Stderr,
				"uninstall: warning: could not deregister the device from the control plane using %s (%v).\n"+
					"           The device may still appear in the web admin; revoke it there if needed.\n", dir, err)
			return
		}
		if outcome == deauth.OutcomeDone {
			fmt.Println("uninstall: device deregistered from the control plane.")
			return
		}
	}
}

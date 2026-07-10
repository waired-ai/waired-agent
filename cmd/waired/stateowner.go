package main

import (
	"fmt"
	"os"

	"github.com/waired-ai/waired-agent/internal/platform/service"
)

// fixStateOwnership is a seam so tests can assert the ownership hand-off
// without needing root / an installed systemd unit.
var fixStateOwnership = service.FixStateOwnership

// handStateToServiceUser hands a root-written state dir back to the
// waired-agent service user (#484). Several `waired` subcommands are run by a
// human via sudo (root) — that root is a second writer into the daemon's state
// dir, so the files it creates land root-owned and the unprivileged
// User=waired daemon then can't read its 0600 secrets or manage its runtime
// dirs. This reclaims the tree for the service user (root:root → waired:waired;
// file modes unchanged).
//
// Best-effort: warns (with an actionable chown hint) but never fails the
// command. No-op unless we are root and the systemd unit is installed
// (FixStateOwnership self-guards), and on macOS/Windows.
func handStateToServiceUser(stateDir string) {
	if err := fixStateOwnership(stateDir); err != nil {
		fmt.Fprintf(os.Stderr,
			"warn: could not hand %s to the waired-agent service user (%v); "+
				"if the daemon can't see its state, run `sudo chown -R waired:waired %s`\n",
			stateDir, err, stateDir)
	}
}

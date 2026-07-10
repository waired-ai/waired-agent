//go:build linux

package update

import (
	"context"
	"os/exec"
	"strings"
)

// LatestVersion resolves the candidate waired version from the configured
// apt suite — the version `apt-get install --only-upgrade waired` would
// install — via `apt-cache policy waired`. This is read-only and needs no
// root, so the unprivileged daemon can run it. It reflects the local apt
// cache (refreshed by the system's daily apt timers); the actual apply
// re-resolves via install.sh, so a momentarily stale candidate self-corrects.
//
// Falls back to the GitHub Releases API when apt is unavailable or the
// package is unknown (a non-apt Linux install), so the check still works.
func (r *Resolver) LatestVersion(ctx context.Context) (string, error) {
	if v := aptCandidate(r.aptPolicy(ctx)); v != "" {
		return v, nil
	}
	return r.latestFromGitHub(ctx)
}

func (r *Resolver) aptPolicy(ctx context.Context) string {
	out, err := r.run(ctx, "apt-cache", "policy", "waired")
	if err != nil {
		return ""
	}
	return out
}

func (r *Resolver) run(ctx context.Context, name string, args ...string) (string, error) {
	if r.runCommand != nil {
		return r.runCommand(ctx, name, args...)
	}
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// aptCandidate extracts the "Candidate: <ver>" value from `apt-cache policy`
// output, returning "" when absent or "(none)".
func aptCandidate(policy string) string {
	for line := range strings.SplitSeq(policy, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Candidate:"); ok {
			v := strings.TrimSpace(rest)
			if v == "" || v == "(none)" {
				return ""
			}
			return v
		}
	}
	return ""
}

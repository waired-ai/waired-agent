//go:build linux

package update

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// resolveLatest resolves the candidate waired version from the configured
// apt suite — the version `apt-get install --only-upgrade waired` would
// install — via `apt-cache policy waired`. This is read-only and needs no
// root, so the unprivileged daemon can run it.
//
// What it reads is the index this host last DOWNLOADED, not what the repo
// publishes now, and nothing on the check path refreshes that index:
// `apt-get update` needs root, and the daemon has none. So the answer is
// only as current as IndexRefreshedAt says it is, which is why that instant
// is reported alongside it (waired-agent#726 — an edge host answered with
// its own installed build for a suite that was six builds ahead). The apply
// path re-resolves through install.sh, which does refresh under elevation.
//
// Falls back to the GitHub Releases API when apt is unavailable or the
// package is unknown (a non-apt Linux install), so the check still works.
// That path is live, so it reports no index instant.
func (r *Resolver) resolveLatest(ctx context.Context, res *Result) error {
	if v := aptCandidate(r.aptPolicy(ctx)); v != "" {
		res.Latest = v
		res.LatestSource = SourceAPT
		res.IndexRefreshedAt = r.aptIndexRefreshedAt()
		return nil
	}
	latest, err := r.latestFromGitHub(ctx)
	if err != nil {
		return err
	}
	res.Latest = latest
	res.LatestSource = SourceGitHub
	return nil
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

// aptSourceLists are the two mutually-exclusive source files install.sh
// writes (linux_apt_ensure_repo in packaging/install/install.sh). A host
// tracks exactly one channel, so whichever exists names the suite whose
// index backs the candidate above. Edge first: it is the channel that
// moves often enough for staleness to matter.
var aptSourceLists = []string{
	"etc/apt/sources.list.d/waired-edge.list",
	"etc/apt/sources.list.d/waired.list",
}

// aptIndexRefreshedAt reports when apt last downloaded the index for the
// waired suite this host tracks. Zero time when it cannot be determined —
// no waired source list (a non-apt install), an unparseable one, or no
// matching index on disk.
//
// Everything it touches is world-readable: install.sh writes the source
// list 0644 and /var/lib/apt/lists is 0755 with 0644 contents. That is what
// lets the unprivileged daemon answer "how old is this answer?" even though
// it cannot answer "make it fresh".
func (r *Resolver) aptIndexRefreshedAt() time.Time {
	suite := aptSuite(r.readAptSourceList())
	if suite == "" {
		return time.Time{}
	}
	// apt names a downloaded index after the URI it came from, with "/"
	// turned into "_" — so the suite is the one part we can predict without
	// re-implementing that escaping, and a glob covers the rest. InRelease
	// is the inline-signed form modern apt fetches; Release is the older
	// split form. Take the newest of whatever is present.
	var newest time.Time
	for _, pattern := range []string{"*_dists_" + suite + "_InRelease", "*_dists_" + suite + "_Release"} {
		matches, err := filepath.Glob(filepath.Join(r.root(), "var/lib/apt/lists", pattern))
		if err != nil {
			continue // only ErrBadPattern, which these patterns are not
		}
		for _, m := range matches {
			fi, err := os.Stat(m)
			if err == nil && fi.ModTime().After(newest) {
				newest = fi.ModTime()
			}
		}
	}
	return newest
}

// readAptSourceList returns the contents of whichever waired source list
// exists, or "" when neither does.
func (r *Resolver) readAptSourceList() string {
	for _, rel := range aptSourceLists {
		if b, err := os.ReadFile(filepath.Join(r.root(), rel)); err == nil {
			return string(b)
		}
	}
	return ""
}

// aptSuite extracts the suite from a one-line deb source entry:
//
//	deb [signed-by=/etc/apt/keyrings/x.gpg arch=amd64] https://host/path <suite> <component>
//
// The bracketed options block contains spaces, so it is removed before the
// line is split rather than counted as fields — otherwise "arch=amd64]"
// would be read as the suite.
func aptSuite(list string) string {
	for line := range strings.SplitSeq(list, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "deb ") && !strings.HasPrefix(line, "deb\t") {
			continue // comment, blank, or deb-src
		}
		if i := strings.Index(line, "["); i >= 0 {
			j := strings.Index(line[i:], "]")
			if j < 0 {
				continue // unterminated options block: not a line we understand
			}
			line = line[:i] + line[i+j+1:]
		}
		if f := strings.Fields(line); len(f) >= 3 {
			return f[2]
		}
	}
	return ""
}

// root is the filesystem root the apt files are read under: "/" in
// production, a fixture tree under test.
func (r *Resolver) root() string {
	if r.aptRoot != "" {
		return r.aptRoot
	}
	return "/"
}

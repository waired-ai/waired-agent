package installscripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// publicInstallerScripts are the files a user who has never seen this
// repository reads the output of. Everything they are pointed at has to be
// reachable without a GitHub account on the private monorepo.
var publicInstallerScripts = []string{
	"packaging/install/install.sh",
	"packaging/install/install.ps1",
	"packaging/install/uninstall.sh",
	"packaging/install/uninstall.ps1",
}

// privateRepoURL matches a link into waired-ai/waired -- the private
// monorepo -- while leaving waired-ai/waired-agent (this public repo) alone.
// Go's regexp has no lookahead, so the boundary is spelled as "any character
// that cannot continue the repo name".
var privateRepoURL = regexp.MustCompile(`github\.com/waired-ai/waired([^-A-Za-z0-9]|$)`)

// TestInstallerScriptsLinkOnlyToPublicPlaces is the regression guard for
// waired-agent#798 (b).
//
// The closing banner told every user "Quickstart:
// https://github.com/waired-ai/waired/blob/main/docs/quickstarts/README.md",
// which 404s for anyone outside the org: waired-ai/waired is private. The
// same file also sent Fedora/RHEL users to that repo's issue tracker. Both
// were printed, both were unreachable, and nothing failed.
func TestInstallerScriptsLinkOnlyToPublicPlaces(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range publicInstallerScripts {
		t.Run(rel, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			for i, line := range strings.Split(string(b), "\n") {
				if m := privateRepoURL.FindString(line); m != "" {
					t.Errorf("%s:%d links into the private monorepo (%q): point users at "+
						"https://docs.waired.ai/ or github.com/waired-ai/waired-agent instead\n  %s",
						rel, i+1, m, strings.TrimSpace(line))
				}
			}
		})
	}
}

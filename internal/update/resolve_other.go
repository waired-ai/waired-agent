//go:build !linux

package update

import "context"

// LatestVersion resolves the latest published stable version via the
// mirror's GitHub Releases API (Windows/macOS). User decision, #293: apt
// query is Linux-only; everywhere else uses the GitHub feed.
func (r *Resolver) LatestVersion(ctx context.Context) (string, error) {
	return r.latestFromGitHub(ctx)
}

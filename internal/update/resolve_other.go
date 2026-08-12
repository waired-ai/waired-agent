//go:build !linux

package update

import "context"

// resolveLatest resolves the latest published stable version via the
// mirror's GitHub Releases API (Windows/macOS). User decision, #293: apt
// query is Linux-only; everywhere else uses the GitHub feed.
//
// The feed is queried live, so there is no local index to go stale and no
// IndexRefreshedAt to report — the staleness these platforms can have is
// the daemon's own result cache, which --force already bypasses.
func (r *Resolver) resolveLatest(ctx context.Context, res *Result) error {
	latest, err := r.latestFromGitHub(ctx)
	if err != nil {
		return err
	}
	res.Latest = latest
	res.LatestSource = SourceGitHub
	return nil
}

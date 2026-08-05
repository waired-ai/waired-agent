//go:build integration

package runtime

import (
	"context"
	"testing"
)

// TestPinnedReleasePublishesEveryAssetChecksum is the bump check, made
// mechanical.
//
// Since #492 the installer verifies each archive against the release's own
// sha256sum.txt, which is why bumping OllamaPinnedVersion needs no hash
// recomputed here (contrast uv, whose UVPinnedSHA256Linux64 does). The
// price of that is two assumptions the installer treats as FATAL rather
// than degrading around: the file exists, and it lists the asset we are
// about to download. Both are properties of the pinned release, so they
// want checking once at bump time — not by every install in the field, and
// not by a comment nobody reruns.
//
// It fails in the two ways a bump actually breaks:
//
//   - upstream stops publishing sha256sum.txt (every install on every OS
//     hard-fails, and this says so before the release ships);
//   - an asset is renamed. 0.30.x renamed the Linux archive from .tgz to
//     .tar.zst — a change that 404s exactly one OS, which the other two
//     legs of a CI run would happily stay green through.
//
// Behind `integration` because it reaches the network: `make
// integration-runtime`, or by hand on the bump PR.
func TestPinnedReleasePublishesEveryAssetChecksum(t *testing.T) {
	sums, err := NewOllamaInstaller(t.TempDir()).fetchChecksums(context.Background())
	if err != nil {
		t.Fatalf("fetching the checksum list for the pinned release v%s: %v\n"+
			"Every install on every OS fails closed without it — see OllamaPinnedVersion's doc.",
			OllamaPinnedVersion, err)
	}

	// Every (goos, goarch) pair we ship a binary for. Kept explicit rather
	// than derived from runtime.GOOS: the point is to catch a rename that
	// only affects the OS this test is NOT running on.
	for _, host := range []struct{ goos, goarch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "arm64"},
		{"windows", "amd64"},
	} {
		rel, err := ollamaReleaseFor(host.goos, host.goarch)
		if err != nil {
			t.Errorf("ollamaReleaseFor(%s, %s): %v", host.goos, host.goarch, err)
			continue
		}
		for _, asset := range []string{rel.Base, rel.ROCm} {
			if asset == "" {
				continue // that OS publishes no ROCm overlay
			}
			if sums[asset] == "" {
				t.Errorf("v%s publishes no checksum for %s (%s/%s) — either the asset was "+
					"renamed upstream or the release is incomplete; the installer refuses to "+
					"extract an asset it cannot verify",
					OllamaPinnedVersion, asset, host.goos, host.goarch)
			}
		}
	}
}

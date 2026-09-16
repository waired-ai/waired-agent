//go:build linux && integration

package runtime

import (
	"context"
	"debug/elf"
	"net/http"
	"testing"
	"time"
)

// TestPinnedUVReleaseResolvesForEveryArch downloads the real pinned uv
// tarball for each architecture the resolver supports, through the same
// code the installer runs: the SHA256 pin must match the release, the
// member must be where the resolver looks, and it must be a binary for
// that machine. arm64 has no host in CI, so this is how its pin
// (UVPinnedSHA256LinuxARM64, waired-ai/waired#1435) is checked on a bump.
// Run with `make integration-runtime`.
func TestPinnedUVReleaseResolvesForEveryArch(t *testing.T) {
	for _, tc := range []struct {
		goarch  string
		machine elf.Machine
	}{
		{"amd64", elf.EM_X86_64},
		{"arm64", elf.EM_AARCH64},
	} {
		t.Run(tc.goarch, func(t *testing.T) {
			r := &UVResolver{
				Root:       t.TempDir(),
				HTTPClient: &http.Client{Timeout: 5 * time.Minute},
				GOARCH:     tc.goarch,
			}
			path, err := r.Resolve(context.Background())
			if err != nil {
				t.Fatalf("Resolve %s: %v", tc.goarch, err)
			}
			f, err := elf.Open(path)
			if err != nil {
				t.Fatalf("resolved uv is not an ELF binary: %v", err)
			}
			defer f.Close()
			if f.Machine != tc.machine {
				t.Errorf("uv for %s is %v, want %v", tc.goarch, f.Machine, tc.machine)
			}
		})
	}
}

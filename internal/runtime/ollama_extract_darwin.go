//go:build darwin

package runtime

import (
	"fmt"
	"os/exec"
	"strings"
)

// extractOllamaArchive unpacks the gzip tar upstream ships for macOS
// (ollama-darwin.tgz) into destDir.
//
// It shells out to the HOST tar, and that is the whole point rather than
// an implementation convenience. The archive is written by bsdtar on
// Apple's own toolchain and carries three things Go's archive/tar would
// silently drop on the floor:
//
//   - symlinks — eight soname links (libggml.dylib -> libggml.0.dylib …)
//     that the engine's dynamic loader follows;
//   - com.apple.cs.CodeSignature / CodeDirectory extended attributes,
//     which carry the signature of the Metal shader library;
//   - AppleDouble "._mlx.metallib" members, which bsdtar merges back into
//     the file they belong to and any other tar leaves as literal junk.
//
// macOS ships bsdtar as /usr/bin/tar, so it reads back exactly what wrote
// the archive. The gzip layer is left to tar as well (-z) — unlike Linux,
// where zstd has to be decoded in-process because the host tar may not
// know the format.
func extractOllamaArchive(archivePath, destDir string) error {
	cmd := exec.Command("tar", "-xzf", archivePath, "-C", destDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

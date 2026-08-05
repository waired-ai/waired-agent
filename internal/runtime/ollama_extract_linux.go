//go:build linux

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/klauspost/compress/zstd"
)

// extractOllamaArchive unpacks a zstd-compressed tar (the ollama-linux
// 0.30+ release layout: bin/ollama + lib/ollama/...) into destDir.
//
// The zstd layer is decompressed IN-PROCESS (klauspost/compress — no zstd
// binary required on the host) and streamed into the system tar via stdin,
// so symlink and permission semantics stay identical to the old
// `tar -xzf` path and the multi-GB decompressed stream never lands in
// memory or on disk as a whole.
func extractOllamaArchive(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	zr, err := zstd.NewReader(f)
	if err != nil {
		return fmt.Errorf("zstd: %w", err)
	}
	defer zr.Close()
	cmd := exec.Command("tar", "-xf", "-", "-C", destDir)
	cmd.Stdin = zr
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

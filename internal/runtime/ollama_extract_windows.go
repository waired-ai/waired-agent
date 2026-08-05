//go:build windows

package runtime

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// extractOllamaArchive unpacks the Windows release ZIP (ollama.exe +
// lib/ollama/{cuda_v12,cuda_v13,vulkan}/…) into destDir.
//
// In Go rather than through a shelled-out expander: Expand-Archive is a
// PowerShell cmdlet, and getting off PowerShell is most of what #493 is
// for. A ZIP also has no symlinks and no extended attributes to preserve,
// which is what makes the host-tool argument that applies on macOS not
// apply here.
//
// The archive is read from disk, not from memory: zip.Reader needs random
// access to the central directory, and the Windows base archive is ~1.5 GB.
func extractOllamaArchive(archivePath, destDir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("zip: %w", err)
	}
	defer func() { _ = zr.Close() }()
	for _, f := range zr.File {
		if err := extractZipEntry(f, destDir); err != nil {
			return fmt.Errorf("zip: %s: %w", f.Name, err)
		}
	}
	return nil
}

// extractZipEntry writes one archive member under destDir.
func extractZipEntry(f *zip.File, destDir string) error {
	dest, err := zipEntryPath(destDir, f.Name)
	if err != nil {
		return err
	}
	if f.FileInfo().IsDir() {
		return os.MkdirAll(dest, 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	// O_TRUNC, not "remove then create": an antivirus scanner holding a
	// read handle on the previous version fails the delete but tolerates
	// the truncate.
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode().Perm()|0o200)
	if err != nil {
		return err
	}
	//nolint:gosec // G110: the member sizes are pinned by the checksum the
	// caller verified before calling us, so there is no attacker-chosen
	// decompression ratio here.
	_, cerr := io.Copy(out, rc)
	if err := out.Close(); cerr == nil {
		cerr = err
	}
	return cerr
}

// zipEntryPath resolves an archive member name against destDir, refusing
// anything that would escape it (the "zip slip" traversal). Pure, so the
// refusal is testable without building an archive.
func zipEntryPath(destDir, name string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive member escapes the install directory")
	}
	return filepath.Join(destDir, clean), nil
}

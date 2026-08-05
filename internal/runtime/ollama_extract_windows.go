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
//
// fresh means "this archive is the whole install": it is extracted BESIDE
// destDir and swapped in only once every entry has landed. Windows earns
// that where the other two OSes do not. A running engine holds a mandatory
// lock on ollama.exe and on every DLL it has loaded, and antivirus commonly
// holds a handle on the directory itself — so an in-place extract can fail
// half way and strand a tree that still has an ollama.exe in it, which the
// next run would read as a complete install and never repair (#190). The
// swap keeps the failure mode "nothing changed" instead.
func extractOllamaArchive(archivePath, destDir string, fresh bool) error {
	if !fresh {
		// The ROCm overlay only adds lib/ollama/rocm on top of a base that is
		// already in place. Staging and swapping it would delete the base.
		return unzipInto(archivePath, destDir)
	}

	staged := destDir + ".new"
	if err := os.RemoveAll(staged); err != nil {
		return fmt.Errorf("clear the staging tree %s: %w", staged, err)
	}
	defer func() { _ = os.RemoveAll(staged) }()
	if err := unzipInto(archivePath, staged); err != nil {
		return err
	}
	return promoteStagedInstall(staged, destDir)
}

// promoteStagedInstall replaces destDir's contents with an already-extracted
// staging tree. The two sit side by side on the same volume, so each move is
// a rename rather than a copy.
//
// destDir itself is kept rather than renamed, for the same reason its
// contents are cleared one entry at a time: antivirus holding a handle fails
// a directory rename but tolerates a move into it.
func promoteStagedInstall(staged, destDir string) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	// Clear first, so a DLL an older version shipped and this one dropped
	// cannot be loaded by the new binary.
	entries, err := os.ReadDir(destDir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(destDir, e.Name())); err != nil {
			return fmt.Errorf("clear the previous install: %w", err)
		}
	}
	moved, err := os.ReadDir(staged)
	if err != nil {
		return err
	}
	for _, e := range moved {
		src := filepath.Join(staged, e.Name())
		dst := filepath.Join(destDir, e.Name())
		if err := os.Rename(src, dst); err != nil {
			// Half a swap leaves a tree that still looks installed. Take it
			// out rather than leave the next run to trust it.
			_ = os.RemoveAll(destDir)
			return fmt.Errorf("install %s: %w", e.Name(), err)
		}
	}
	return nil
}

// unzipInto writes every member of the archive under destDir.
func unzipInto(archivePath, destDir string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("zip: %w", err)
	}
	defer func() { _ = zr.Close() }()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
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

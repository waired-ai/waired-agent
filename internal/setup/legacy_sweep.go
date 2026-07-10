package setup

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Legacy v1 sentinel markers — kept here, in the new setup package, so
// the v1 → v2 migration sweep keeps working after `internal/integration/shell`
// is deleted. Do NOT reuse these strings for anything new; they are
// frozen in the contract that an existing v1 install put into a user's
// rc files. The new `# >>> waired-claude alias` markers live in
// `internal/integration/shellalias`.
const (
	legacyManagedOpen  = "# >>> waired managed (do not edit) >>>"
	legacyManagedClose = "# <<< waired managed <<<"
)

// SweepLegacyManagedBlocks removes the legacy `# >>> waired managed`
// sentinel block from any rc file under homeDir. Returns the absolute
// paths of files that were modified.
//
// The block is the env.sh-sourcing snippet that v1 installs wrote into
// `~/.bashrc` / `~/.zshrc` / `~/.config/fish/config.fish`. Once the
// snippet's env.sh disappears (v2 no longer writes one), an unswept
// block becomes inert noise — but a freshly-cloned dotfile that *does*
// keep an old env.sh around will keep injecting stale ANTHROPIC_BASE_URL
// values, the very class of incident the wrapper subcommand was built
// to prevent. Sweep on every `waired unlink` so v1 users get
// cleaned up on their first v2 uninstall.
//
// Errors on individual files are swallowed: the helper is best-effort
// migration support, not a guarantee. Caller decides how loudly to
// surface the result.
func SweepLegacyManagedBlocks(homeDir string) ([]string, error) {
	if homeDir == "" {
		return nil, errors.New("setup: SweepLegacyManagedBlocks: empty homeDir")
	}
	candidates := []string{
		filepath.Join(homeDir, ".bashrc"),
		filepath.Join(homeDir, ".zshrc"),
		filepath.Join(homeDir, ".config", "fish", "config.fish"),
	}
	var changed []string
	for _, path := range candidates {
		removed, err := removeLegacyManagedBlock(path)
		if err != nil {
			// Skip but don't abort — other rc files may still be
			// cleanable, and a single unreadable file should not
			// block the operator's uninstall flow.
			continue
		}
		if removed {
			changed = append(changed, path)
		}
	}
	return changed, nil
}

// removeLegacyManagedBlock excises the sentinel-bracketed legacy block
// from path, atomically rewriting the file. Returns (true, nil) when a
// block was found and removed, (false, nil) when no block was present
// or the file does not exist, (false, err) on read/write errors that
// the caller should not silently ignore.
func removeLegacyManagedBlock(path string) (bool, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	start, end, ok := findLegacyManagedSpan(body)
	if !ok {
		return false, nil
	}
	out := append([]byte{}, body[:start]...)
	out = append(out, body[end:]...)
	out = collapseDoubleBlankLines(out)
	if err := atomicWriteFile(path, out, 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// findLegacyManagedSpan locates the `[start, end)` byte range of the
// legacy sentinel block within data. The end is positioned past the
// trailing newline of the close marker (when present) so the block can
// be excised cleanly.
//
// The scan is line-oriented (so inline mentions of the marker string
// inside a comment elsewhere don't confuse us) and only matches the
// first complete open→close pair. Stray opens without a matching close
// are left alone — the safer default is "no change" rather than
// destroying the rest of the file.
func findLegacyManagedSpan(data []byte) (int, int, bool) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	offset := 0
	startSet := false
	var start int
	for scanner.Scan() {
		line := scanner.Bytes()
		lineLen := len(line) + 1 // scanner strips the trailing \n
		trimmed := bytes.TrimSpace(line)
		switch {
		case !startSet && bytes.Equal(trimmed, []byte(legacyManagedOpen)):
			start = offset
			startSet = true
		case startSet && bytes.Equal(trimmed, []byte(legacyManagedClose)):
			return start, offset + lineLen, true
		}
		offset += lineLen
	}
	return 0, 0, false
}

// collapseDoubleBlankLines shrinks "...\n\n\n..." back to "...\n\n",
// which is the typical residue left behind when a leading-blank-line
// block is removed.
func collapseDoubleBlankLines(data []byte) []byte {
	for {
		i := bytes.Index(data, []byte("\n\n\n"))
		if i < 0 {
			return data
		}
		data = append(data[:i+1], data[i+2:]...)
	}
}

// atomicWriteFile writes data to path via tmp+rename so a crashed write
// never leaves a half-edited rc file behind.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".waired-rc-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

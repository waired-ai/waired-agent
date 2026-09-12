package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// promoteStagedInstall replaces destDir's contents with an already-extracted
// staging tree. The two sit side by side on the same volume, so each move is
// a rename rather than a copy.
//
// destDir itself is kept rather than renamed, for the same reason its
// contents are cleared one entry at a time: antivirus holding a handle fails
// a directory rename but tolerates a move into it.
//
// Renaming is also what makes the install ATOMIC from the outside, which
// is the property waired-agent#1309 is about. The daemon decides an engine
// is installed by stat-ing one path and finding a regular file there
// (engine_resolve.go's resolveOllamaBinary), and it polls for that every
// two seconds. An extractor writing in place makes the binary a regular
// file the moment tar opens it, minutes before the bytes are all there —
// so the daemon exec'd a half-written Mach-O and reported "The inference
// engine won't start." A rename publishes the file complete or not at all.
//
// exclusive says destDir holds nothing but the engine payload, and
// therefore that anything in it which this archive does not carry is a
// leftover of the version being replaced. That is true where the payload
// unpacks into its own bin/ directory (macOS, Windows) and false on Linux,
// where destDir is the install's base directory and also holds the model
// store, the engine's logs and the HOME it is spawned with. Clearing that
// wholesale would delete tens of GB of weights, so the Linux case clears
// only the top-level names the staged tree is about to occupy — which
// still drops a stale library from inside lib/, because the whole
// directory is replaced.
func promoteStagedInstall(staged, destDir string, exclusive bool) error {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	moved, err := os.ReadDir(staged)
	if err != nil {
		return err
	}
	// Clear first, so a library an older version shipped and this one
	// dropped cannot be loaded by the new binary.
	stale, err := staleEntries(staged, destDir, exclusive)
	if err != nil {
		return err
	}
	for _, name := range stale {
		if err := os.RemoveAll(filepath.Join(destDir, name)); err != nil {
			return fmt.Errorf("clear the previous install: %w", err)
		}
	}
	for _, e := range moved {
		src := filepath.Join(staged, e.Name())
		dst := filepath.Join(destDir, e.Name())
		if err := os.Rename(src, dst); err != nil {
			// Half a swap leaves a tree that still looks installed. Take
			// out what this promotion was replacing rather than leave the
			// next run to trust it — but only that, never a neighbour's
			// data on a shared destDir.
			for _, name := range stale {
				_ = os.RemoveAll(filepath.Join(destDir, name))
			}
			for _, m := range moved {
				_ = os.RemoveAll(filepath.Join(destDir, m.Name()))
			}
			return fmt.Errorf("install %s: %w", e.Name(), err)
		}
	}
	return nil
}

// staleEntries is the names promoteStagedInstall removes from destDir
// before moving the staged tree in.
//
// Split out and pure-ish so the two rules are table-testable together:
// this is the one decision in the promotion that differs per OS, and
// getting it wrong on Linux deletes the model store.
func staleEntries(staged, destDir string, exclusive bool) ([]string, error) {
	if exclusive {
		entries, err := os.ReadDir(destDir)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		return names, nil
	}
	entries, err := os.ReadDir(staged)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

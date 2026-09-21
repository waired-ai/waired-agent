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
//
// The previous install is moved aside rather than deleted, and moved back
// if anything after that fails, so a promotion either completes or leaves
// the previous version exactly as it was (waired-agent#1511). Deleting it
// first had one way to go wrong on every OS and a second on Windows: a
// running ollama.exe cannot be deleted, and since the entries are cleared
// in name order, lib/ was already gone when that failed — a live server
// with no libraries and nothing restored. Windows does let a running
// executable be renamed, so moving it aside succeeds there.
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
	// Beside the staged tree, so each move is a rename on one volume.
	aside := staged + ".previous"
	if err := os.MkdirAll(aside, 0o700); err != nil {
		return err
	}
	var setAside, movedIn []string
	restore := func() {
		for _, name := range movedIn {
			_ = os.RemoveAll(filepath.Join(destDir, name))
		}
		for _, name := range setAside {
			_ = renameFn(filepath.Join(aside, name), filepath.Join(destDir, name))
		}
	}
	for _, name := range stale {
		src := filepath.Join(destDir, name)
		if _, err := os.Lstat(src); os.IsNotExist(err) {
			continue
		}
		if err := renameFn(src, filepath.Join(aside, name)); err != nil {
			restore()
			return fmt.Errorf("move the previous install aside: %w", err)
		}
		setAside = append(setAside, name)
	}
	for _, e := range moved {
		if err := renameFn(filepath.Join(staged, e.Name()), filepath.Join(destDir, e.Name())); err != nil {
			restore()
			return fmt.Errorf("install %s: %w", e.Name(), err)
		}
		movedIn = append(movedIn, e.Name())
	}
	// Best effort: a file of the previous version still in use stays until
	// the staging directory is swept by the next install.
	_ = os.RemoveAll(aside)
	return nil
}

// renameFn is os.Rename, a seam so a test can fail one step of a promotion.
var renameFn = os.Rename

// staleEntries is the names promoteStagedInstall moves out of destDir
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

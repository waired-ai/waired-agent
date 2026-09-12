//go:build integration

package runtime_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	wrt "github.com/waired-ai/waired-agent/internal/runtime"
)

// TestOllamaInstaller_RealArchive installs the pinned engine release into
// a temporary directory and checks the three things a fake extractor
// cannot (waired-agent#1309).
//
// Opt-in (build tag `integration`) and network-bound: it downloads the
// real ~160 MB asset. Nothing outside the temp directory is touched —
// no state dir, no daemon, no model store — so it is safe to run on a
// host that is serving.
//
//	go test -tags integration -run TestOllamaInstaller_RealArchive -v ./internal/runtime/...
//
// What it adds over the unit tests:
//
//  1. The daemon's poll never sees a partial binary. The watcher below
//     stats BinaryPath() every 2 ms for the whole install, which is what
//     setup_desired.go does every 2 s — against the REAL extractor, on
//     the real archive. Before the staging change, an in-place untar of a
//     ~1 GB universal Mach-O published that path seconds before the bytes
//     were there and the daemon exec'd it.
//  2. The payload survives promotion. os.Rename should preserve symlinks
//     and extended attributes, and on macOS the archive carries eight
//     soname links the dynamic loader follows and a CodeSignature xattr
//     on the Metal shader library. "Should" is not evidence about a
//     signature; running the binary is.
//  3. The promoted binary actually executes.
func TestOllamaInstaller_RealArchive(t *testing.T) {
	base := t.TempDir()
	inst := wrt.NewOllamaInstaller(base)

	var (
		partialSeen atomic.Bool
		partialSize atomic.Int64
		stop        = make(chan struct{})
		watching    = make(chan struct{})
	)
	go func() {
		close(watching)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Exactly the predicate engine_resolve.go's
			// resolveOllamaBinary uses: a regular file at this path.
			if fi, err := os.Stat(inst.BinaryPath()); err == nil && fi.Mode().IsRegular() {
				partialSeen.Store(true)
				partialSize.Store(fi.Size())
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	<-watching

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	var stages []string
	if err := inst.Install(ctx, func(p wrt.OllamaInstallProgress) {
		if len(stages) == 0 || stages[len(stages)-1] != p.Stage {
			stages = append(stages, p.Stage)
		}
	}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	close(stop)
	t.Logf("stages: %v", stages)

	// The watcher stops at the first sighting, so a sighting here is
	// either the finished binary (fine — the install is over) or a
	// partial one. Tell them apart by size: the real binary is hundreds
	// of MB, and the watcher records what it saw.
	final, err := os.Stat(inst.BinaryPath())
	if err != nil {
		t.Fatalf("no binary at %s after install: %v", inst.BinaryPath(), err)
	}
	if partialSeen.Load() && partialSize.Load() != final.Size() {
		t.Errorf("the daemon's path held a %d-byte file mid-install; the finished binary is %d bytes",
			partialSize.Load(), final.Size())
	}
	if !inst.Active() {
		t.Errorf("Active() false after install")
	}

	// (3) It runs. On macOS this is the assertion that carries the
	// CodeSignature question: a Mach-O whose signature did not survive
	// the move is killed by the kernel rather than reported by a file
	// listing.
	out, err := exec.CommandContext(ctx, inst.BinaryPath(), "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", inst.BinaryPath(), err, out)
	}
	t.Logf("promoted binary answers: %s", strings.TrimSpace(string(out)))

	// (2) Symlinks and xattrs, reported rather than only asserted: the
	// counts are what this run observed, and a future release changing
	// them is information rather than a failure.
	links, withXattr := 0, 0
	root := base
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			links++
		}
		if runtime.GOOS == "darwin" && !d.IsDir() {
			if b, xerr := exec.Command("/usr/bin/xattr", p).Output(); xerr == nil && strings.Contains(string(b), "com.apple.") {
				withXattr++
			}
		}
		return nil
	})
	t.Logf("after promotion: %d symlinks, %d files carrying an com.apple.* xattr", links, withXattr)
	if runtime.GOOS == "darwin" && links == 0 {
		t.Error("no symlinks survived promotion; the macOS archive carries soname links the loader follows")
	}
}

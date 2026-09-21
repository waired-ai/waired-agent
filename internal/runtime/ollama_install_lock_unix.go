//go:build linux || darwin

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// ollamaInstallLockName is the sidecar Lock flocks, beside the engine
// under BaseDir. It is outside every name an install replaces (bin/ and
// lib/ on Linux, bin/'s contents on macOS), so no promotion touches it.
const ollamaInstallLockName = ".install.lock"

// ollamaLockPoll is how often Lock asks again while another process holds it.
const ollamaLockPoll = 500 * time.Millisecond

// Lock serialises installs of the bundled engine under BaseDir across
// processes: the daemon's start-up converge and the CLI's install and
// upgrade (waired-agent#1511). On the apt path the postinst restarts the
// daemon, whose converge starts a download, and then install.sh runs the
// CLI's; the two shared one staging directory, deleted each other's
// archives and failed each other's checksums. The caller that takes the
// lock probes, decides and installs under it (ConvergeOllama re-probes once
// it holds it). onWait, when set, is called once if the lock is busy.
//
// The sidecar is opened read-only when this process cannot write it (it is
// root's and this is the service user): flock needs no write access.
func (i *OllamaInstaller) Lock(ctx context.Context, onWait func()) (func(), error) {
	if err := os.MkdirAll(i.BaseDir, 0o755); err != nil {
		return nil, fmt.Errorf("ollama install lock: %w", err)
	}
	path := filepath.Join(i.BaseDir, ollamaInstallLockName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		if f, err = os.Open(path); err != nil {
			return nil, fmt.Errorf("ollama install lock: %w", err)
		}
	}
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() {
				_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("ollama install lock %s: %w", path, err)
		}
		if onWait != nil {
			onWait()
			onWait = nil
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("ollama install lock %s: another install is still running: %w", path, ctx.Err())
		case <-time.After(ollamaLockPoll):
		}
	}
}

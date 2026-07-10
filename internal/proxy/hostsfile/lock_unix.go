//go:build linux || darwin

package hostsfile

import (
	"os"

	"golang.org/x/sys/unix"
)

// withHostsLock serializes the read-modify-write inside Add/Remove against a
// concurrent editor of the same hosts file. Both Linux and macOS create
// multiple root processes that edit /etc/hosts: on Linux the systemd drop-in's
// ExecStartPost/ExecStopPost AND the proxy-converge .path oneshot; on macOS the
// proxy-converge LaunchDaemon (WatchPaths) AND `waired proxy uninstall`'s
// hosts-remove. An agent restart that coincides with a tray/CLI toggle can put
// two root processes mid read-modify-write at once. Manager.write rewrites in
// place with O_TRUNC (to preserve owner/permissions/ACL — it does not rename),
// so without a lock the two can lost-update the redirect block or interleave a
// torn write on a system file.
//
// The lock is an advisory flock(LOCK_EX) held on a sidecar file beside the
// hosts file for the duration of fn. Failure to create or acquire the lock
// (e.g. the directory is not writable) is non-fatal: we proceed unlocked rather
// than refuse the edit, preserving the previous best-effort behaviour. The
// sidecar is left in place between edits — it is an empty 0-byte file and
// re-locking it is the whole point.
func withHostsLock(path string, fn func() error) error {
	f, err := os.OpenFile(path+".waired-lock", os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fn() // best-effort: cannot create the lock file
	}
	defer func() { _ = f.Close() }()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fn() // best-effort: cannot acquire the lock
	}
	defer func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN) }()
	return fn()
}

//go:build darwin

package logrotate

import (
	"fmt"
	"os"
	"syscall"
)

func defaultOps() ops { return ops{reopen: reopen, sameFile: sameFile} }

// reopen points fd at path's current inode, creating the file if the
// rotation just moved it away.
//
// dup2 rather than swapping the slog handler's io.Writer: the
// descriptor number is what everything else in the process writes
// through — direct os.Stderr writes, the Go runtime's panic output, and
// any child that inherited it. Re-pointing the number covers all of
// them at once, and os.Stderr (a wrapper around fd 2) keeps working
// untouched.
//
// The temporary *os.File is closed on the way out; dup2 duplicated the
// open file description onto fd, so fd keeps it alive.
func reopen(path string, fd int) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Dup2(int(f.Fd()), fd); err != nil {
		return fmt.Errorf("dup2 %d -> %d: %w", f.Fd(), fd, err)
	}
	return nil
}

// sameFile reports whether fd currently refers to the file at path,
// comparing (device, inode).
//
// Deliberately raw fstat(2) rather than os.NewFile(fd, …) + os.SameFile:
// os.NewFile attaches a finalizer that closes the descriptor when the
// wrapper is collected, and the descriptors here are 1 and 2. Borrowing
// them through os would eventually close the process's own stdout or
// stderr.
func sameFile(fd int, path string) (bool, error) {
	var open syscall.Stat_t
	if err := syscall.Fstat(fd, &open); err != nil {
		return false, fmt.Errorf("fstat fd %d: %w", fd, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	onDisk, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("stat %s: no syscall.Stat_t", path)
	}
	return open.Dev == onDisk.Dev && open.Ino == onDisk.Ino, nil
}

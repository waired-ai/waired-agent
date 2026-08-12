// Package atomicfile publishes a file by renaming a staged temp file over
// it, and keeps that working on Windows.
//
// The write-temp-then-rename shape is used all over this repo because on
// Unix the rename is atomic and cannot be observed half-done. On Windows it
// has a failure mode Unix does not: the rename is refused outright while
// EITHER file is open — including by a plain reader, because Go's
// os.OpenFile does not request FILE_SHARE_DELETE. One concurrent reader of
// the file being replaced is enough.
//
// Measured on real NTFS with a GOOS=windows build
// (docs/knowledges/20260812/0120-windows-rename-open-handle.md):
//
//	source held open            FAIL  "used by another process"
//	destination held for write  FAIL  "Access is denied."
//	destination held for read   FAIL  "Access is denied."
//	neither held                OK
//
// So the error is not a sign of a broken host, and a green re-run is not a
// sign the race is gone. It is a window of microseconds, and a retry closes
// it (waired-agent#698).
package atomicfile

import (
	"os"
	"time"
)

// replaceAttempts and replacePause bound the retry. The window being waited
// out is one reader's open-read-close of a small file, so the pause is far
// larger than the window; the attempt count is what carries it through a
// reader descheduled on a loaded runner. Up to ~200 ms in total, which is
// short beside the 5 s heartbeat the busiest caller runs on.
//
// A record of today's behaviour, not a contract — no issue fixes these
// figures, and the only cost of raising them is a slower failure on a host
// where the file is genuinely unwritable.
const (
	replaceAttempts = 40
	replacePause    = 5 * time.Millisecond
)

// Replace renames oldpath over newpath, retrying while the platform reports
// the rename was refused because a handle is open on one of the two files.
// On every OS but Windows there is nothing to retry and this is one
// os.Rename.
func Replace(oldpath, newpath string) error {
	return replaceWithRetry(
		func() error { return os.Rename(oldpath, newpath) },
		replaceBlocked,
		time.Sleep,
		replaceAttempts,
		replacePause,
	)
}

// replaceWithRetry is the decision, kept below the OS: rename until it
// succeeds, fails for a reason retrying cannot fix, or runs out of attempts.
// rename, blocked and sleep are arguments so the loop is table-tested on any
// OS and without waiting — the only per-OS part left is which errors
// blocked() accepts.
func replaceWithRetry(rename func() error, blocked func(error) bool, sleep func(time.Duration), attempts int, pause time.Duration) error {
	var err error
	for i := 0; i < attempts; i++ {
		err = rename()
		if err == nil || !blocked(err) {
			return err
		}
		if i < attempts-1 {
			sleep(pause)
		}
	}
	return err
}

package atomicfile

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// replaceBlocked reports whether err is Windows refusing the replacing
// rename because a handle is open on one of the two files.
//
// Two errno values, and which one it is says which side is held (the
// measured table in the package comment):
//
//	ERROR_ACCESS_DENIED     the DESTINATION is open — by a writer or, just
//	                        as fatally, by a reader
//	ERROR_SHARING_VIOLATION the SOURCE, the staged temp file, is open
//
// Both are transient by construction here: the writer owns the temp file
// and releases it before renaming, and a reader of the destination holds it
// only for the length of one read. Anything else — a real permission
// problem, a missing directory — is not in this set, so it returns
// immediately instead of spending the retry budget.
//
// windows.ERROR_SHARING_VIOLATION rather than a syscall constant: the
// syscall package does not export that one, and x/sys/windows is already a
// dependency of this module.
func replaceBlocked(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == syscall.ERROR_ACCESS_DENIED || errno == syscall.Errno(windows.ERROR_SHARING_VIOLATION)
}

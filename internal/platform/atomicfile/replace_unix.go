//go:build linux || darwin

package atomicfile

// replaceBlocked is always false on the Unixes: rename(2) replaces the
// destination regardless of who has either file open, and an open handle
// keeps reading the inode it was opened on. There is nothing here for a
// retry to wait out, so Replace makes exactly one os.Rename call.
func replaceBlocked(error) bool { return false }

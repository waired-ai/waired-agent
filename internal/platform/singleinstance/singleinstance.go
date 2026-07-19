// Package singleinstance provides a best-effort, cross-process guard so
// that only one copy of a given named application runs at a time.
//
// It exists for waired-tray: several launch surfaces can fire in quick
// succession — the installer's post-install launch, the Start-menu
// shortcut, and the HKCU Run autostart at the next logon — and without a
// guard each one registers its own notification-area icon, leaving the
// user with several identical Waired icons (waired#807).
//
// The mechanism is per-OS:
//   - Windows: a named mutex in the per-session Local\ namespace
//     (singleinstance_windows.go).
//   - Linux/macOS: an advisory flock held on a lock file under the
//     per-user state dir (singleinstance_unix.go).
package singleinstance

// Acquire attempts to become the single live instance for name.
//
// On success it returns ok=true and a release func; the caller holds the
// guard for the process lifetime and calls release on shutdown. release
// is always non-nil and safe to call (it is a no-op when there is nothing
// to release), so `defer release()` is always valid. The OS also drops
// the mutex / flock when the process exits, so a missed release is not
// fatal.
//
// ok=false means another live instance already holds the guard: the
// caller should exit silently with status 0 and do nothing else.
//
// A non-nil err reports that the guard could not be established at all
// (e.g. the lock directory is not writable). This is deliberately
// non-fatal: err is returned together with ok=true so the caller can log
// it and proceed *unguarded* rather than refuse to start. A guard failure
// must never stop the app from running.
func Acquire(name string) (release func(), ok bool, err error) {
	return acquire(name)
}

// noop is the release returned whenever there is no handle/fd to close.
func noop() {}

//go:build windows

package singleinstance

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

// acquire creates a named mutex in the per-session Local\ namespace. The
// Local\ prefix scopes the name to the current logon session, which is
// what we want: the guard is per-user (matching the per-user launch
// surfaces — Start-menu shortcut, HKCU Run autostart), not machine-wide.
func acquire(name string) (func(), bool, error) {
	return acquireMutex(`Local\` + name)
}

// acquireMutex creates the named mutex. CreateMutex returns a valid
// handle even when the object already exists; the distinguishing signal
// is ERROR_ALREADY_EXISTS in the returned error (golang.org/x/sys/windows
// surfaces it specifically for this case). The handle is held open for
// the process lifetime — Windows destroys the mutex object when the last
// handle closes, so keeping ours open is what makes the guard live.
func acquireMutex(objectName string) (func(), bool, error) {
	namePtr, err := windows.UTF16PtrFromString(objectName)
	if err != nil {
		return noop, true, fmt.Errorf("singleinstance: mutex name %q: %w", objectName, err)
	}
	handle, err := windows.CreateMutex(nil, false, namePtr)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		// Another live instance already owns the mutex.
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return noop, false, nil
	}
	if err != nil {
		// Could not create the mutex at all — proceed unguarded.
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return noop, true, fmt.Errorf("singleinstance: create mutex %q: %w", objectName, err)
	}
	return func() { _ = windows.CloseHandle(handle) }, true, nil
}

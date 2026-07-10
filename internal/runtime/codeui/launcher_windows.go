//go:build windows

package codeui

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachSysProcAttr starts the serve host detached from the launching console
// so it survives the parent exiting.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
	}
}

// signalStop terminates the serve host. Windows has no SIGTERM for an
// unrelated process group, so Kill is the portable stop; the host's deferred
// teardown still removes runtime.json on its way out where possible.
func signalStop(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

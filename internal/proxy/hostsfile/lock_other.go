//go:build !linux && !darwin

package hostsfile

// withHostsLock is a no-op on Windows, where the hosts file is edited in-process
// by the single LocalSystem agent (already serialized — see the Windows proxy
// lifecycle). The unix build (lock_unix.go) provides the real flock
// implementation, which guards against the concurrent root editors systemd
// (Linux) and the converge LaunchDaemon (macOS) create.
func withHostsLock(_ string, fn func() error) error {
	return fn()
}

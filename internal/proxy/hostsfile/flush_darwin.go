//go:build darwin

package hostsfile

import "os/exec"

// runFlushCmd runs one resolver-flush command. A package var so tests can
// observe the invocations without shelling out to the real resolver.
var runFlushCmd = func(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

// flushDNS invalidates the macOS DNS resolver cache after the hosts redirect
// block changes, so api.anthropic.com re-resolves immediately rather than after
// the cached TTL. dscacheutil -flushcache clears the directory-services cache
// and SIGHUP makes mDNSResponder re-read its configuration. Both are
// best-effort: killall needs root (the converge LaunchDaemon has it); when run
// unprivileged the flush merely delays propagation instead of failing the edit.
func flushDNS() {
	_ = runFlushCmd("dscacheutil", "-flushcache")
	_ = runFlushCmd("killall", "-HUP", "mDNSResponder")
}

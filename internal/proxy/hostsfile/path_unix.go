//go:build !windows

package hostsfile

// DefaultPath is the hosts file on Linux and macOS. Only used by production
// wiring; tests pass an explicit temp path.
const DefaultPath = "/etc/hosts"

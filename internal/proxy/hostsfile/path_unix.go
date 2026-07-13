//go:build linux || darwin

package hostsfile

// DefaultPath is the hosts file on Linux and macOS. Only used by production
// wiring; tests pass an explicit temp path.
const DefaultPath = "/etc/hosts"

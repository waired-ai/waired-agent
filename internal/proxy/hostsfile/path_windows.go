//go:build windows

package hostsfile

// DefaultPath is the Windows hosts file. Only used by production wiring; tests
// pass an explicit temp path.
const DefaultPath = `C:\Windows\System32\drivers\etc\hosts`

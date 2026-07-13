//go:build !linux && !windows && !darwin

package hostsfile

// flushDNS compile stub for GOOS values we don't ship; the shipped OSes
// have their own flush_<goos>.go.
func flushDNS() {}

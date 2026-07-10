//go:build !windows && !darwin

package hostsfile

// flushDNS is a no-op on Linux: glibc/musl resolve through nsswitch and read
// /etc/hosts on every lookup, so there is no resolver cache to invalidate after
// editing the block. Windows (flush_windows.go) and macOS (flush_darwin.go)
// have a resolver cache that must be flushed.
func flushDNS() {}

//go:build windows

package hostsfile

import "golang.org/x/sys/windows"

var (
	dnsapi               = windows.NewLazySystemDLL("dnsapi.dll")
	procDNSFlushResolver = dnsapi.NewProc("DnsFlushResolverCache")
)

// flushDNS clears the Windows DNS Client resolver cache so a freshly
// added/removed hosts entry for api.anthropic.com takes effect immediately
// rather than after a previously cached resolution expires. This is the call
// `ipconfig /flushdns` makes internally.
//
// Best-effort by contract: a missing export or a flush failure only delays
// propagation slightly (the hosts file is still authoritative on the next
// uncached lookup), so it never blocks or fails the hosts edit.
func flushDNS() {
	if err := procDNSFlushResolver.Find(); err != nil {
		return
	}
	_, _, _ = procDNSFlushResolver.Call()
}

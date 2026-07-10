//go:build darwin

package netif

import (
	"net"
	"net/netip"
	"strings"
	"testing"
)

// TestLocalCandidates_RealDarwinHost is a live-host sanity check that
// runs only under GOOS=darwin. It pulls the real interface list via
// net.Interfaces() and verifies that LocalCandidates() never emits a
// candidate whose addr belongs to one of the Apple-internal pseudo
// interfaces (awdl*, llw*, pktap*) — even if the host happens to be
// in an unusual state where one of those interfaces carries a
// routable-looking address.
//
// This catches a class of regressions that the synthetic snapshot
// test cannot: a future macOS release that starts handing out global
// addresses on awdl0, or a change in net.Interfaces() result shape.
//
// On hosts where none of these interfaces are present at all the
// assertion trivially passes; the test does not require any specific
// macOS feature to be active.
func TestLocalCandidates_RealDarwinHost(t *testing.T) {
	// Build the addr → iface-name lookup from the live kernel view.
	addrToIface := map[string]string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("net.Interfaces: %v", err)
	}
	for _, ifi := range ifaces {
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ip := addrFromNetAddr(a)
			if !ip.IsValid() {
				continue
			}
			addrToIface[ip.String()] = ifi.Name
		}
	}

	// Ask netif for everything it would advertise, with all gates
	// open so we have the biggest possible surface to inspect.
	cands := LocalCandidates(Options{
		ListenPort:     41820,
		IncludeIPv6:    true,
		IncludeULA:     true,
		IncludeIPv4LAN: true,
	})

	for _, c := range cands {
		raw := stripEndpointPrefix(c.Addr)
		host, _, err := net.SplitHostPort(raw)
		if err != nil {
			t.Errorf("malformed candidate addr %q: %v", c.Addr, err)
			continue
		}
		host = strings.Trim(host, "[]")
		ip, err := netip.ParseAddr(host)
		if err != nil {
			t.Errorf("candidate addr %q is not parseable: %v", c.Addr, err)
			continue
		}
		ifname := addrToIface[ip.String()]
		switch {
		case strings.HasPrefix(ifname, "awdl"),
			strings.HasPrefix(ifname, "llw"),
			strings.HasPrefix(ifname, "pktap"):
			t.Errorf("candidate %q lives on %q, which must be skipped by name on darwin", c.Addr, ifname)
		}
	}

	t.Logf("emitted %d candidate(s) across %d live interface(s); none on awdl*/llw*/pktap*",
		len(cands), len(ifaces))
}

// stripEndpointPrefix removes the "udp4:" / "udp6:" prefix from a
// netif candidate.Addr so net.SplitHostPort can parse the rest.
func stripEndpointPrefix(s string) string {
	for _, p := range []string{"udp4:", "udp6:"} {
		if strings.HasPrefix(s, p) {
			return strings.TrimPrefix(s, p)
		}
	}
	return s
}

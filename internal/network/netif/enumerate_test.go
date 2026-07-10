package netif

import (
	"net"
	"net/netip"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/proto/signer"
)

const wgPort = uint16(41820)

func mustAddr(s string) netip.Addr {
	a, err := netip.ParseAddr(s)
	if err != nil {
		panic(err)
	}
	return a
}

// snapshotFromAddrs is a tiny test helper that builds a snapshot
// containing a single non-loopback up interface carrying addrs.
func snapshotFromAddrs(addrs []string, preferredV6 string) snapshot {
	s := snapshot{}
	if preferredV6 != "" {
		s.PreferredIPv6 = mustAddr(preferredV6)
	}
	infos := ifaceInfo{
		Name:  "eth0",
		Flags: net.FlagUp,
	}
	for _, a := range addrs {
		infos.Addrs = append(infos.Addrs, mustAddr(a))
	}
	s.Interfaces = []ifaceInfo{infos}
	return s
}

func TestEnumerate_IPv6GUAEmittedWhenReachable(t *testing.T) {
	got := enumerate(
		snapshotFromAddrs([]string{"2001:db8::5"}, "2001:db8::5"),
		Options{ListenPort: wgPort, IncludeIPv6: true},
	)
	want := []controlclient.CandidateAdvertise{
		{Addr: "udp6:[2001:db8::5]:41820", Kind: signer.KindIPv6, Priority: 2},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestEnumerate_IPv6SuppressedWhenNoDefaultRoute(t *testing.T) {
	// preferredV6 empty == no v6 default route. GUA must NOT be
	// advertised even if the interface carries a routable addr.
	got := enumerate(
		snapshotFromAddrs([]string{"2001:db8::5"}, ""),
		Options{ListenPort: wgPort, IncludeIPv6: true},
	)
	if len(got) != 0 {
		t.Fatalf("expected zero candidates without v6 default route, got %+v", got)
	}
}

func TestEnumerate_ULASuppressedByDefault(t *testing.T) {
	got := enumerate(
		snapshotFromAddrs([]string{"fd00:1::1", "2001:db8::5"}, "2001:db8::5"),
		Options{ListenPort: wgPort, IncludeIPv6: true},
	)
	for _, c := range got {
		if strings.Contains(c.Addr, "fd00:1::1") {
			t.Fatalf("ULA fd00:1::1 must not appear without IncludeULA: %+v", got)
		}
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly the GUA, got %+v", got)
	}
}

func TestEnumerate_ULAEmittedWhenIncluded(t *testing.T) {
	got := enumerate(
		snapshotFromAddrs([]string{"fd00:1::1", "2001:db8::5"}, "2001:db8::5"),
		Options{ListenPort: wgPort, IncludeIPv6: true, IncludeULA: true},
	)
	if len(got) != 2 {
		t.Fatalf("expected GUA + ULA, got %+v", got)
	}
	addrs := []string{got[0].Addr, got[1].Addr}
	if !contains(addrs, "udp6:[2001:db8::5]:41820") {
		t.Fatalf("missing GUA: %+v", addrs)
	}
	if !contains(addrs, "udp6:[fd00:1::1]:41820") {
		t.Fatalf("missing ULA: %+v", addrs)
	}
}

func TestEnumerate_LinkLocalAlwaysFiltered(t *testing.T) {
	got := enumerate(
		snapshotFromAddrs([]string{"fe80::1", "169.254.1.1"}, "2001:db8::5"),
		Options{
			ListenPort:     wgPort,
			IncludeIPv6:    true,
			IncludeULA:     true,
			IncludeIPv4LAN: true,
		},
	)
	if len(got) != 0 {
		t.Fatalf("link-local must always be filtered, got %+v", got)
	}
}

func TestEnumerate_LoopbackAndUnspecifiedFiltered(t *testing.T) {
	got := enumerate(
		snapshotFromAddrs([]string{"::1", "::", "127.0.0.1", "0.0.0.0"}, "2001:db8::5"),
		Options{
			ListenPort:     wgPort,
			IncludeIPv6:    true,
			IncludeULA:     true,
			IncludeIPv4LAN: true,
		},
	)
	if len(got) != 0 {
		t.Fatalf("loopback/unspecified must be filtered, got %+v", got)
	}
}

func TestEnumerate_LoopbackInterfaceSkipped(t *testing.T) {
	s := snapshot{
		PreferredIPv6: mustAddr("2001:db8::5"),
		Interfaces: []ifaceInfo{
			{
				Name:  "lo",
				Flags: net.FlagUp | net.FlagLoopback,
				Addrs: []netip.Addr{mustAddr("2001:db8::5")},
			},
		},
	}
	got := enumerate(s, Options{ListenPort: wgPort, IncludeIPv6: true})
	if len(got) != 0 {
		t.Fatalf("loopback interface must be skipped wholesale, got %+v", got)
	}
}

func TestEnumerate_DownInterfaceSkipped(t *testing.T) {
	s := snapshot{
		PreferredIPv6: mustAddr("2001:db8::5"),
		Interfaces: []ifaceInfo{
			{
				Name:  "eth0",
				Flags: 0, // down
				Addrs: []netip.Addr{mustAddr("2001:db8::5")},
			},
		},
	}
	got := enumerate(s, Options{ListenPort: wgPort, IncludeIPv6: true})
	if len(got) != 0 {
		t.Fatalf("down interface must be skipped, got %+v", got)
	}
}

func TestEnumerate_DarwinInternalInterfacesSkippedByName(t *testing.T) {
	// awdl0 / llw0 / pktap0 are Apple-internal pseudo-interfaces
	// (AirDrop / Continuity / packet capture). Even when synthesised
	// with a routable-looking GUA they must be dropped by name on
	// darwin before classify() ever sees the addr. On non-darwin
	// builds the name filter is a no-op, so the same five interfaces
	// would all emit; we test both directions.
	s := snapshot{
		PreferredIPv6: mustAddr("2001:db8::1"),
		Interfaces: []ifaceInfo{
			{Name: "awdl0", Flags: net.FlagUp, Addrs: []netip.Addr{mustAddr("2001:db8::10")}},
			{Name: "llw0", Flags: net.FlagUp, Addrs: []netip.Addr{mustAddr("2001:db8::11")}},
			{Name: "pktap0", Flags: net.FlagUp, Addrs: []netip.Addr{mustAddr("2001:db8::12")}},
			{Name: "en0", Flags: net.FlagUp, Addrs: []netip.Addr{mustAddr("2001:db8::20")}},
			{Name: "utun0", Flags: net.FlagUp, Addrs: []netip.Addr{mustAddr("2001:db8::21")}},
		},
	}
	got := enumerate(s, Options{ListenPort: wgPort, IncludeIPv6: true})

	addrs := map[string]bool{}
	for _, c := range got {
		addrs[c.Addr] = true
	}

	// en0 + utun0 must always appear: en0 is a normal NIC and utun*
	// is left intact because Tailscale / iCloud Private Relay /
	// corporate VPN clients all live in the utun namespace and their
	// addresses are valid peer endpoints.
	for _, want := range []string{
		"udp6:[2001:db8::20]:41820", // en0
		"udp6:[2001:db8::21]:41820", // utun0
	} {
		if !addrs[want] {
			t.Errorf("expected candidate missing: %s (got %+v)", want, got)
		}
	}

	forbidden := []string{
		"udp6:[2001:db8::10]:41820", // awdl0
		"udp6:[2001:db8::11]:41820", // llw0
		"udp6:[2001:db8::12]:41820", // pktap0
	}
	if runtime.GOOS == "darwin" {
		for _, a := range forbidden {
			if addrs[a] {
				t.Errorf("forbidden candidate emitted on darwin: %s (must be skipped by name; got %+v)", a, got)
			}
		}
		if len(got) != 2 {
			t.Errorf("darwin: expected exactly 2 candidates (en0+utun0), got %d (%+v)", len(got), got)
		}
	} else {
		for _, a := range forbidden {
			if !addrs[a] {
				t.Errorf("non-darwin: name filter must be a no-op, but %s did not emit (got %+v)", a, got)
			}
		}
		if len(got) != 5 {
			t.Errorf("non-darwin: expected 5 candidates (no name filter), got %d (%+v)", len(got), got)
		}
	}
}

func TestEnumerate_IPv4LANGated(t *testing.T) {
	withFlag := enumerate(
		snapshotFromAddrs([]string{"192.168.1.42"}, ""),
		Options{ListenPort: wgPort, IncludeIPv4LAN: true},
	)
	if len(withFlag) != 1 || withFlag[0].Addr != "udp4:192.168.1.42:41820" {
		t.Fatalf("IPv4 LAN should be emitted with flag, got %+v", withFlag)
	}
	if withFlag[0].Kind != signer.KindLocal {
		t.Fatalf("IPv4 LAN must be Kind=local, got %q", withFlag[0].Kind)
	}

	withoutFlag := enumerate(
		snapshotFromAddrs([]string{"192.168.1.42"}, ""),
		Options{ListenPort: wgPort},
	)
	if len(withoutFlag) != 0 {
		t.Fatalf("IPv4 LAN should be suppressed without flag, got %+v", withoutFlag)
	}
}

func TestEnumerate_PublicIPv4Suppressed(t *testing.T) {
	// Even with IPv4LAN enabled, a public v4 is not netif's job —
	// relay STUN observes it. Filter it.
	got := enumerate(
		snapshotFromAddrs([]string{"198.51.100.10"}, ""),
		Options{ListenPort: wgPort, IncludeIPv4LAN: true},
	)
	if len(got) != 0 {
		t.Fatalf("public v4 must be suppressed, got %+v", got)
	}
}

func TestEnumerate_IPv6KillSwitch(t *testing.T) {
	// Even with a routable GUA + default route, IncludeIPv6=false
	// must produce nothing for v6.
	got := enumerate(
		snapshotFromAddrs([]string{"2001:db8::5"}, "2001:db8::5"),
		Options{ListenPort: wgPort, IncludeIPv6: false},
	)
	if len(got) != 0 {
		t.Fatalf("IncludeIPv6=false should suppress v6, got %+v", got)
	}
}

func TestEnumerate_ZeroListenPortReturnsNil(t *testing.T) {
	got := enumerate(
		snapshotFromAddrs([]string{"2001:db8::5"}, "2001:db8::5"),
		Options{ListenPort: 0, IncludeIPv6: true},
	)
	if got != nil {
		t.Fatalf("zero ListenPort should return nil, got %+v", got)
	}
}

func TestEnumerate_DeduplicatesAcrossInterfaces(t *testing.T) {
	// Multi-homed host where the same v6 GUA appears on two ifaces
	// (some Linux setups expose this via additional /32 routes).
	// We must emit one candidate, not two.
	s := snapshot{
		PreferredIPv6: mustAddr("2001:db8::5"),
		Interfaces: []ifaceInfo{
			{Name: "eth0", Flags: net.FlagUp, Addrs: []netip.Addr{mustAddr("2001:db8::5")}},
			{Name: "eth1", Flags: net.FlagUp, Addrs: []netip.Addr{mustAddr("2001:db8::5")}},
		},
	}
	got := enumerate(s, Options{ListenPort: wgPort, IncludeIPv6: true})
	if len(got) != 1 {
		t.Fatalf("expected one deduped candidate, got %+v", got)
	}
}

func TestEnumerate_OrdersIPv6BeforeIPv4LAN(t *testing.T) {
	got := enumerate(
		snapshotFromAddrs([]string{"192.168.1.42", "2001:db8::5"}, "2001:db8::5"),
		Options{ListenPort: wgPort, IncludeIPv6: true, IncludeIPv4LAN: true},
	)
	if len(got) != 2 {
		t.Fatalf("expected 2 candidates, got %+v", got)
	}
	if got[0].Kind != signer.KindIPv6 {
		t.Fatalf("v6 must come first; got %+v", got)
	}
	if got[1].Kind != signer.KindLocal {
		t.Fatalf("v4 LAN must come second; got %+v", got)
	}
}

func TestIsAdvertisableIPv6(t *testing.T) {
	cases := []struct {
		s          string
		includeULA bool
		want       bool
	}{
		{"2001:db8::5", false, true},
		{"fd00:1::1", false, false},
		{"fd00:1::1", true, true},
		{"fe80::1", true, false},
		{"::1", true, false},
		{"192.168.1.1", true, false},
		{"not-an-addr", true, false},
	}
	for _, tc := range cases {
		if got := IsAdvertisableIPv6(tc.s, tc.includeULA); got != tc.want {
			t.Errorf("IsAdvertisableIPv6(%q, ula=%v) = %v, want %v",
				tc.s, tc.includeULA, got, tc.want)
		}
	}
}

// contains is a tiny slice helper local to this test file.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

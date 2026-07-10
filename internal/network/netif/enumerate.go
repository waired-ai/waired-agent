// Package netif enumerates the host's reachable network interface
// addresses and emits them as AdvertiseEndpoints candidates so the
// control plane can hand them to peers.
//
// Today the disco subsystem advertises only the NAT-mapped public addr
// learned from the relay STUN echo. That leaves the agent's other
// candidates — IPv4 LAN addresses and globally routable IPv6 — invisible
// to peers, which forces every connection through the relay until disco
// happens to converge. netif fills that gap.
//
// Scope:
//   - IPv6 GUA (2000::/3) — emitted as KindIPv6 when a working v6
//     default route exists (Dial-probed once per snapshot).
//   - IPv6 ULA (fc00::/7) — emitted as KindIPv6 only when
//     Options.IncludeULA is set. Off by default because ULAs are not
//     routable on the public internet and waste probe cycles.
//   - IPv4 LAN (RFC1918) — emitted as KindLocal when
//     Options.IncludeIPv4LAN is set.
//
// Out of scope:
//   - Link-local (fe80::/10, 169.254.0.0/16): a candidate string has
//     no scope-id field, peers cannot dial it.
//   - Loopback, unspecified, multicast: never reachable from peers.
//   - UPnP / NAT-PMP / PCP: deferred per
//     feedback_no_upnp / docs/specs/waired_client_network_spec.md.
package netif

import (
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Options governs what LocalCandidates emits.
type Options struct {
	// ListenPort is the agent's WG UDP listen port. Every emitted
	// candidate carries this port — peers will dial it directly.
	ListenPort uint16

	// IncludeIPv6 is the kill switch for the v6 path. Defaults to
	// true at the caller; setting false skips both GUA and ULA.
	IncludeIPv6 bool

	// IncludeULA emits fc00::/7 candidates as KindIPv6. Off by
	// default — ULAs are useful inside a campus but unreachable
	// from typical public-internet peers.
	IncludeULA bool

	// IncludeIPv4LAN emits RFC1918 / link-local-v4-free LAN
	// addresses as KindLocal. Off by default: today's reconciler
	// already gets a useful IPv4 candidate from relay-STUN.
	IncludeIPv4LAN bool
}

// LocalCandidates inspects the live host and returns the set of
// reachable candidates as AdvertiseEndpoints entries. Safe to call
// concurrently — it does not hold state. Callers refresh
// periodically; CP rate-limits duplicates.
func LocalCandidates(opts Options) []controlclient.CandidateAdvertise {
	return enumerate(snapshot{
		Interfaces:    liveInterfaces(),
		PreferredIPv6: probeIPv6Default(),
	}, opts)
}

// snapshot is the host state Enumerate consumes — extracted so tests
// can drive arbitrary topologies without touching the kernel.
type snapshot struct {
	Interfaces []ifaceInfo
	// PreferredIPv6 is the kernel-preferred source GUA for outbound
	// v6 traffic, learned by Dial("udp6", public-DNS:53) +
	// LocalAddr(). Zero value when no v6 default route exists,
	// which short-circuits all GUA emission.
	PreferredIPv6 netip.Addr
}

// ifaceInfo is the per-interface input enumerate works on.
type ifaceInfo struct {
	Name  string
	Flags net.Flags
	Addrs []netip.Addr
}

// enumerate is the pure decision function — exported only for tests
// via the test-package boundary.
func enumerate(s snapshot, opts Options) []controlclient.CandidateAdvertise {
	if opts.ListenPort == 0 {
		return nil
	}
	v6Reachable := opts.IncludeIPv6 && s.PreferredIPv6.IsValid()
	out := []controlclient.CandidateAdvertise{}
	// Sort interfaces deterministically so the order doesn't churn
	// CP cache hits across snapshots that differ only by Go map
	// iteration.
	ifaces := append([]ifaceInfo(nil), s.Interfaces...)
	sort.Slice(ifaces, func(i, j int) bool { return ifaces[i].Name < ifaces[j].Name })
	seen := map[netip.Addr]struct{}{}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		// Defense in depth: drop platform-internal pseudo-interfaces
		// (darwin awdl/llw/pktap) by name, even if their addresses
		// happen to pass classify(). See interfaces_darwin.go.
		if shouldSkipInterfaceByName(ifi.Name) {
			continue
		}
		// Sort addrs within an interface for stable ordering.
		addrs := append([]netip.Addr(nil), ifi.Addrs...)
		sort.Slice(addrs, func(i, j int) bool { return addrs[i].Less(addrs[j]) })
		for _, ip := range addrs {
			if _, dup := seen[ip]; dup {
				continue
			}
			cand, ok := classify(ip, opts, v6Reachable)
			if !ok {
				continue
			}
			seen[ip] = struct{}{}
			cand.Addr = formatAddr(ip, opts.ListenPort)
			out = append(out, cand)
		}
	}
	// Stable final ordering: KindIPv6 first (preferred path),
	// KindLocal second; within each kind, addr string ascending.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return kindRank(out[i].Kind) < kindRank(out[j].Kind)
		}
		return out[i].Addr < out[j].Addr
	})
	return out
}

func kindRank(k string) int {
	switch k {
	case signer.KindIPv6:
		return 0
	case signer.KindLocal:
		return 1
	default:
		return 99
	}
}

// classify decides whether a single addr should be emitted and as
// which Kind. It returns a zero-Addr CandidateAdvertise the caller
// fills in with the formatted addr string.
func classify(ip netip.Addr, opts Options, v6Reachable bool) (controlclient.CandidateAdvertise, bool) {
	if !ip.IsValid() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
		return controlclient.CandidateAdvertise{}, false
	}
	if ip.IsLinkLocalUnicast() {
		return controlclient.CandidateAdvertise{}, false
	}
	if ip.Is6() {
		if !v6Reachable {
			return controlclient.CandidateAdvertise{}, false
		}
		if isULA(ip) && !opts.IncludeULA {
			return controlclient.CandidateAdvertise{}, false
		}
		if !ip.IsGlobalUnicast() && !isULA(ip) {
			return controlclient.CandidateAdvertise{}, false
		}
		return controlclient.CandidateAdvertise{Kind: signer.KindIPv6, Priority: 2}, true
	}
	// IPv4 — only emit RFC1918-ish LAN addresses, and only when
	// the caller opts in.
	if !opts.IncludeIPv4LAN {
		return controlclient.CandidateAdvertise{}, false
	}
	if !ip.IsPrivate() {
		// Public v4 not advertised via netif — relay STUN already
		// observes the NAT-mapped public addr.
		return controlclient.CandidateAdvertise{}, false
	}
	return controlclient.CandidateAdvertise{Kind: signer.KindLocal, Priority: 3}, true
}

// isULA reports whether ip falls in fc00::/7.
func isULA(ip netip.Addr) bool {
	if !ip.Is6() || ip.Is4In6() {
		return false
	}
	b := ip.As16()
	return b[0]&0xfe == 0xfc
}

// formatAddr renders the wgnet endpoint syntax: "udp4:host:port" or
// "udp6:[host]:port". Mirrors disco.Service.makeUDPDst so the format
// stays consistent through advertise → CP → reconciler → bind.Send.
func formatAddr(ip netip.Addr, port uint16) string {
	if ip.Is4() {
		return "udp4:" + ip.String() + ":" + itoa(int(port))
	}
	return "udp6:[" + ip.String() + "]:" + itoa(int(port))
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [6]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

// liveInterfaces enumerates the kernel-visible interfaces and their
// netip-typed addrs. Errors are logged-and-skipped; an empty return
// is normal in tightly-locked sandboxes (e.g. containers without
// CAP_NET_ADMIN).
func liveInterfaces() []ifaceInfo {
	raw, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]ifaceInfo, 0, len(raw))
	for _, ifi := range raw {
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		info := ifaceInfo{Name: ifi.Name, Flags: ifi.Flags}
		for _, a := range addrs {
			ip := addrFromNetAddr(a)
			if ip.IsValid() {
				info.Addrs = append(info.Addrs, ip)
			}
		}
		out = append(out, info)
	}
	return out
}

func addrFromNetAddr(a net.Addr) netip.Addr {
	switch v := a.(type) {
	case *net.IPNet:
		ip, ok := netip.AddrFromSlice(v.IP)
		if !ok {
			return netip.Addr{}
		}
		return ip.Unmap()
	case *net.IPAddr:
		ip, ok := netip.AddrFromSlice(v.IP)
		if !ok {
			return netip.Addr{}
		}
		return ip.Unmap()
	}
	return netip.Addr{}
}

// probeIPv6Default learns the kernel-preferred source GUA for v6
// outbound traffic by Dial-ing a well-known public-DNS v6 addr and
// reading LocalAddr(). It never sends a packet (Dial on UDP is
// connectionless / no handshake) but takes care to time-bound so a
// hung resolver / firewall doesn't stall agent startup.
//
// Returns the zero netip.Addr when no v6 default route is reachable,
// which short-circuits all GUA emission upstream.
func probeIPv6Default() netip.Addr {
	d := net.Dialer{Timeout: 200 * time.Millisecond}
	c, err := d.Dial("udp6", "[2001:4860:4860::8888]:53")
	if err != nil {
		return netip.Addr{}
	}
	defer c.Close()
	la, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || la == nil {
		return netip.Addr{}
	}
	ip, ok := netip.AddrFromSlice(la.IP)
	if !ok {
		return netip.Addr{}
	}
	ip = ip.Unmap()
	if !ip.Is6() || ip.IsLinkLocalUnicast() || ip.IsLoopback() {
		return netip.Addr{}
	}
	return ip
}

// IsAdvertisableIPv6 reports whether the given addr is one netif
// would emit as KindIPv6 candidate. Exposed so the agent's startup
// log can summarize which addresses are participating.
func IsAdvertisableIPv6(s string, includeULA bool) bool {
	ip, err := netip.ParseAddr(strings.TrimSpace(s))
	if err != nil {
		return false
	}
	if !ip.Is6() || ip.Is4In6() {
		return false
	}
	cand, ok := classify(ip, Options{IncludeIPv6: true, IncludeULA: includeULA}, true)
	return ok && cand.Kind == signer.KindIPv6
}

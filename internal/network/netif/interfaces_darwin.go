//go:build darwin

package netif

import "strings"

// shouldSkipInterfaceByName reports whether a darwin interface should be
// dropped from candidate emission solely because of its name, before any
// address-level classification runs.
//
// Today's address-based classify() already filters everything these
// interfaces typically carry — awdl0/llw0 expose only IPv6 link-local
// (AirDrop / Continuity bring-ups), pktap* are libpcap pseudo-devices
// with no routable addr. Adding a name-based skip here is defense in
// depth: if a future macOS release ever assigns a routable GUA to one
// of these special-purpose interfaces, we still won't advertise it as
// a peer candidate, because peers cannot actually reach the agent over
// AirDrop / packet-capture pseudo-devices.
//
// utun* is deliberately NOT in this list. The macOS utun namespace is
// used by every userspace VPN (Tailscale, iCloud Private Relay,
// corporate VPN clients, …) and those addresses are perfectly valid
// peer-reachable endpoints for whichever overlay the user is on.
// Filtering utun* would silently break legitimate setups.
func shouldSkipInterfaceByName(name string) bool {
	switch {
	case strings.HasPrefix(name, "awdl"):
		return true
	case strings.HasPrefix(name, "llw"):
		return true
	case strings.HasPrefix(name, "pktap"):
		return true
	}
	return false
}

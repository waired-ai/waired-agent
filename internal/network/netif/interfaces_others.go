//go:build !darwin

package netif

// shouldSkipInterfaceByName is a no-op on non-darwin platforms — the
// darwin build has a real implementation that filters Apple-internal
// pseudo-interfaces (awdl/llw/pktap). Linux and Windows have no
// equivalent "always-skip-by-name" set; the address-based classify()
// already filters everything those platforms' standard interfaces
// carry.
func shouldSkipInterfaceByName(string) bool { return false }

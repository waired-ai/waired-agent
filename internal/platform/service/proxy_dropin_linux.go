//go:build linux

package service

import (
	"fmt"
	"os"
)

// The retired transparent Claude proxy (#488) was layered onto the waired-agent
// unit via a systemd drop-in plus a 127.0.0.1:443 socket-activation unit, plus a
// rootless-toggle converger. Only the REMOVAL path survives, so an upgraded host
// can be cleaned up (internal/proxy/legacycleanup); the install side is gone
// with the MITM proxy.
const (
	proxyDropInDir  = "/etc/systemd/system/" + ServiceName + ".service.d"
	proxyDropInPath = proxyDropInDir + "/10-proxy.conf"
	proxySocketUnit = "waired-agent-proxy.socket"
	proxySocketPath = "/etc/systemd/system/" + proxySocketUnit
)

// RemoveProxyDropIn stops+disables the socket and removes both units, plus the
// rootless-toggle converger units. Missing files are not errors. Best-effort on
// the systemctl calls so a half-installed state can still be cleaned up.
func RemoveProxyDropIn() error {
	firstErr := removeProxyConverge()
	_ = runSystemctl("disable", "--now", proxySocketUnit)
	for _, p := range []string{proxyDropInPath, proxySocketPath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = fmt.Errorf("service: remove %s: %w", p, err)
		}
	}
	// Drop the now-empty drop-in directory (Remove fails harmlessly if other
	// drop-ins still live there).
	_ = os.Remove(proxyDropInDir)
	if err := runSystemctl("daemon-reload"); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

//go:build linux

package service

import (
	"fmt"
	"os"
)

// The retired transparent Claude proxy (#488) had a rootless On/Off toggle
// driven by a systemd .path unit watching desired-proxy plus a oneshot that
// converged /etc/hosts. Only the REMOVAL path survives so an upgraded host can
// be cleaned up (internal/proxy/legacycleanup); the install side is gone with
// the MITM proxy.
const (
	proxyConvergePathUnit    = "waired-agent-proxy-converge.path"
	proxyConvergeServiceUnit = "waired-agent-proxy-converge.service"
	proxyConvergePathPath    = "/etc/systemd/system/" + proxyConvergePathUnit
	proxyConvergeServicePath = "/etc/systemd/system/" + proxyConvergeServiceUnit
)

// removeProxyConverge stops+disables the path watcher and removes both units.
// Best-effort; missing files are not errors. Called from RemoveProxyDropIn.
func removeProxyConverge() error {
	_ = runSystemctl("disable", "--now", proxyConvergePathUnit)
	var firstErr error
	for _, p := range []string{proxyConvergePathPath, proxyConvergeServicePath} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = fmt.Errorf("service: remove %s: %w", p, err)
		}
	}
	return firstErr
}

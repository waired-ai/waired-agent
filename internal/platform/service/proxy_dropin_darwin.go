//go:build darwin

package service

import (
	"fmt"
	"os"
	"path/filepath"
)

// The retired transparent Claude proxy on macOS (#488) was driven by two root
// LaunchDaemons (com.waired.proxy-socket binding 127.0.0.1:443 + a hosts-convergence
// watcher). Only the REMOVAL path survives so an upgraded host can be cleaned
// up (internal/proxy/legacycleanup); the install side is gone with the MITM
// proxy.
const (
	proxySocketLabel   = "com.waired.proxy-socket"
	proxyConvergeLabel = "com.waired.proxy-converge"
)

// systemDaemonDir is where the LaunchDaemon plists live. A var so tests can
// point it at a temp dir (the real path requires root to write).
var systemDaemonDir = "/Library/LaunchDaemons"

func proxySocketPlistPath() string {
	return filepath.Join(systemDaemonDir, proxySocketLabel+".plist")
}
func proxyConvergePlistPath() string {
	return filepath.Join(systemDaemonDir, proxyConvergeLabel+".plist")
}

// RemoveProxyDropIn boots out both legacy proxy daemons and removes their
// plists. Missing files are not errors. Requires root. Best-effort cleanup for
// hosts upgraded off the retired MITM proxy.
func RemoveProxyDropIn() error {
	var firstErr error
	for _, d := range []struct{ label, plist string }{
		{proxySocketLabel, proxySocketPlistPath()},
		{proxyConvergeLabel, proxyConvergePlistPath()},
	} {
		_, _, _ = runLaunchctlFn([]string{"bootout", "system/" + d.label})
		if err := os.Remove(d.plist); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = fmt.Errorf("service: remove %s: %w", d.plist, err)
		}
	}
	return firstErr
}

// Package legacycleanup removes the artifacts of the retired transparent MITM
// Claude proxy (#488) when a host is upgraded to the managed-settings
// ANTHROPIC_BASE_URL integration.
//
// This is not merely tidiness: a stale `api.anthropic.com -> 127.0.0.1` hosts
// redirect left behind by the old proxy would point at a :443 listener that no
// longer exists, AND would loop the new loopback gateway's passthrough leg
// (which now resolves api.anthropic.com via ordinary DNS) back onto a dead
// port — silently breaking Claude Code. Stripping the redirect is therefore
// mandatory on upgrade; removing the now-orphaned MITM CA additionally resolves
// the dangling NODE_EXTRA_CA_CERTS trust bug (#487).
//
// All steps are best-effort and idempotent: a host that never ran the MITM
// proxy returns immediately via the present() fast path, and every removal
// tolerates "already absent". Run must execute with elevation (the init /
// `waired claude disable` root phase); a non-root caller's failures are logged,
// not fatal.
package legacycleanup

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/waired-ai/waired-agent/internal/platform/service"
	"github.com/waired-ai/waired-agent/internal/proxy/hostsfile"
	"github.com/waired-ai/waired-agent/internal/proxy/trust"
)

// Run removes every legacy MITM artifact for stateDir, best-effort. It is a
// no-op (and cheap) when no legacy proxy was ever installed on this host.
// Returns true when it actually cleaned something up.
func Run(stateDir string, logger *slog.Logger) bool {
	if logger == nil {
		logger = slog.Default()
	}
	if !present(stateDir) {
		return false
	}
	logger.Info("removing retired MITM Claude proxy artifacts (migrating to managed-settings)")

	step(logger, "untrust MITM CA", trust.UninstallCA)
	step(logger, "remove NODE_EXTRA_CA_CERTS bridge", trust.UninstallNodeExtraCA)
	step(logger, "strip api.anthropic.com hosts redirect", func() error {
		return hostsfile.New("", nil).Remove()
	})
	step(logger, "remove proxy service units", func() error {
		// No-op / unsupported (e.g. Windows had no service units) is fine.
		if err := service.RemoveProxyDropIn(); err != nil {
			logger.Debug("legacycleanup: RemoveProxyDropIn", "err", err)
		}
		return nil
	})
	step(logger, "remove on-disk MITM CA", func() error {
		return os.RemoveAll(filepath.Join(stateDir, "proxy"))
	})
	return true
}

// Present reports whether this host shows any sign of a legacy MITM Claude
// proxy install (the waired-written on-disk CA under stateDir/proxy, or a live
// api.anthropic.com hosts redirect). Cheap. Exposed so `waired claude status`
// can warn that retired proxy artifacts are still present and point the user at
// `waired claude enable`, which sweeps them (waired#750).
func Present(stateDir string) bool { return present(stateDir) }

// present reports whether this host shows any sign of a legacy MITM install:
// the waired-written on-disk CA, or a live hosts redirect block. Cheap (a stat
// plus one /etc/hosts read).
func present(stateDir string) bool {
	if _, err := os.Stat(filepath.Join(stateDir, "proxy", "ca.crt")); err == nil {
		return true
	}
	if p, _ := hostsfile.New("", nil).Present(); p {
		return true
	}
	return false
}

func step(logger *slog.Logger, what string, fn func() error) {
	if err := fn(); err != nil {
		logger.Warn("legacycleanup: "+what+" failed (best-effort)", "err", err)
	}
}

//go:build linux

package trust

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	// caStoreDir is the local CA-anchor directory consumed by
	// update-ca-certificates on Debian/Ubuntu derivatives.
	caStoreDir = "/usr/local/share/ca-certificates"
	// profilePath sources NODE_EXTRA_CA_CERTS into every login shell.
	profilePath = "/etc/profile.d/waired-claude-proxy.sh"
)

func caStorePath() string { return filepath.Join(caStoreDir, CAStoreFileName) }

// UninstallCA removes the anchor and refreshes the trust store. A missing
// anchor is not an error.
func UninstallCA() error {
	if err := os.Remove(caStorePath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("trust: remove %s: %w", caStorePath(), err)
	}
	return runUpdateCACertificates()
}

// UninstallNodeExtraCA removes the /etc/profile.d snippet. Missing is OK.
func UninstallNodeExtraCA() error {
	if err := os.Remove(profilePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("trust: remove %s: %w", profilePath, err)
	}
	return nil
}

func runUpdateCACertificates() error {
	bin, err := exec.LookPath("update-ca-certificates")
	if err != nil {
		return fmt.Errorf("trust: update-ca-certificates not found (non-Debian host?): %w", err)
	}
	if out, err := exec.Command(bin, "--fresh").CombinedOutput(); err != nil {
		return fmt.Errorf("trust: update-ca-certificates: %w: %s", err, out)
	}
	return nil
}

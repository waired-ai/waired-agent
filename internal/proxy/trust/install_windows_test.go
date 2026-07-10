//go:build windows

package trust

import (
	"os/exec"
	"strings"
	"testing"
)

// TestCertutilAvailable smoke-tests the exec wiring UninstallCA relies
// on without mutating the machine cert store: `certutil -?` prints help and
// exits 0. certutil ships in System32, but skip rather than fail if a stripped
// image lacks it.
func TestCertutilAvailable(t *testing.T) {
	if _, err := exec.LookPath("certutil"); err != nil {
		t.Skip("certutil not on PATH; skipping")
	}
	out, err := runCertutil("-?")
	if err != nil {
		t.Fatalf("certutil -?: %v (out: %s)", err, out)
	}
	if !strings.Contains(strings.ToLower(out), "certutil") {
		t.Errorf("certutil -? output missing usage banner: %q", out)
	}
}

// TestBroadcastEnvChangeNoPanic ensures the WM_SETTINGCHANGE broadcast is safe
// to call (best-effort; it never returns an error by contract).
func TestBroadcastEnvChangeNoPanic(t *testing.T) {
	broadcastEnvChange()
}

// TestConstants guards the locale-independent CRYPT_E_NOT_FOUND code and the
// registry path used for the machine env var.
func TestConstants(t *testing.T) {
	if cryptNotFound != "0x80092004" {
		t.Errorf("cryptNotFound drifted: %q", cryptNotFound)
	}
	if !strings.HasSuffix(envRegPath, `Session Manager\Environment`) {
		t.Errorf("envRegPath not the machine environment block: %q", envRegPath)
	}
	if nodeExtraCAEnv != "NODE_EXTRA_CA_CERTS" {
		t.Errorf("nodeExtraCAEnv drifted: %q", nodeExtraCAEnv)
	}
}

//go:build linux

package controlurl

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/platform/service"
)

// TestLinuxEnvFilePathMatchesService pins linuxEnvFileDir to the systemd
// EnvironmentFile the unit actually names. The constant is duplicated
// here (service.LinuxEnvFilePath is in a //go:build linux file, and this
// package stays untagged so envFileDir can be table-tested over every
// GOOS on any runner), so this is the only thing standing between the two
// and silent drift — a drift would point `waired init` and daemon sign-in
// at a file the installer never writes. Product contract.
func TestLinuxEnvFilePathMatchesService(t *testing.T) {
	if got, want := EnvFilePath("linux", "/ignored"), service.LinuxEnvFilePath; got != want {
		t.Errorf("EnvFilePath(\"linux\", ...) = %q, want service.LinuxEnvFilePath %q", got, want)
	}
}

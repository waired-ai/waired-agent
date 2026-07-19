package paths

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStateDir_EnvOverrideWins(t *testing.T) {
	t.Setenv(EnvOverride, filepath.FromSlash("/tmp/forced"))
	for _, m := range []Mode{AutoDetect, Interactive, System} {
		if got := StateDir(m); got != filepath.FromSlash("/tmp/forced") {
			t.Fatalf("StateDir(%v) = %q, want /tmp/forced (env override)", m, got)
		}
	}
}

func TestStateDir_NonEmpty(t *testing.T) {
	t.Setenv(EnvOverride, "")
	for _, m := range []Mode{AutoDetect, Interactive, System} {
		if got := StateDir(m); got == "" {
			t.Fatalf("StateDir(%v) returned empty string", m)
		}
	}
}

func TestSecretsAndCacheDir(t *testing.T) {
	base := filepath.FromSlash("/var/lib/waired")
	if got, want := SecretsDir(base), filepath.Join(base, "secrets"); got != want {
		t.Fatalf("SecretsDir = %q, want %q", got, want)
	}
	if got, want := CacheDir(base), filepath.Join(base, "cache"); got != want {
		t.Fatalf("CacheDir = %q, want %q", got, want)
	}
}

func TestMgmtEndpoint_NonEmpty(t *testing.T) {
	t.Setenv(MgmtSocketEnvOverride, "")
	for _, m := range []Mode{AutoDetect, Interactive, System} {
		if got := MgmtEndpoint(m); got == "" {
			t.Fatalf("MgmtEndpoint(%v) returned empty string", m)
		}
	}
}

func TestMgmtEndpoint_EnvOverride(t *testing.T) {
	const forced = "/tmp/forced-mgmt.sock"
	t.Setenv(MgmtSocketEnvOverride, forced)
	got := MgmtEndpoint(System)
	if runtime.GOOS == "windows" {
		// Windows uses a fixed named pipe and ignores the socket override.
		if got == forced {
			t.Fatalf("MgmtEndpoint honoured $%s on Windows: %q", MgmtSocketEnvOverride, got)
		}
		return
	}
	if got != forced {
		t.Fatalf("MgmtEndpoint(System) = %q, want %q (env override)", got, forced)
	}
}

// --- waired#81: per-instance endpoints ---------------------------------

func TestSamePath(t *testing.T) {
	cases := []struct {
		name string
		goos string
		a, b string
		want bool
	}{
		{"linux exact", "linux", "/var/lib/waired", "/var/lib/waired", true},
		{"linux uncleaned", "linux", "/var/lib/waired/", "/var/lib/waired", true},
		{"linux case differs", "linux", "/var/lib/Waired", "/var/lib/waired", false},
		{"darwin exact", "darwin", "/Library/Application Support/waired", "/Library/Application Support/waired", true},
		{"darwin case differs", "darwin", "/library/application support/waired", "/Library/Application Support/waired", false},
		// Windows path comparison is case-insensitive, so an override spelled
		// with a different drive/dir case is still the DEFAULT dir and must
		// not be mistaken for a separate instance.
		{"windows case differs", "windows", `C:\ProgramData\waired`, `c:\programdata\WAIRED`, true},
		{"windows genuinely different", "windows", `C:\dev\w1`, `C:\ProgramData\waired`, false},
		{"empty a", "linux", "", "/var/lib/waired", false},
		{"empty b", "linux", "/var/lib/waired", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := samePath(tc.goos, tc.a, tc.b); got != tc.want {
				t.Fatalf("samePath(%q, %q, %q) = %v, want %v", tc.goos, tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestInstanceMgmtEndpoint_DefaultDirsAreNotInstances is the regression guard
// for the trap in waired#81: every CI leg that exports $WAIRED_STATE_DIR
// (installtest-integration.sh, installtest-macos.sh, installtest-windows.ps1)
// points it at the OS DEFAULT state dir, while the daemon is started by
// systemd/launchd/the SCM and never sees the variable. If a default dir were
// treated as an instance, only the client would move and every routing
// sentinel leg would break.
func TestInstanceMgmtEndpoint_DefaultDirsAreNotInstances(t *testing.T) {
	for _, m := range []Mode{System, Interactive} {
		def := osStateDir(m)
		if got := InstanceMgmtEndpoint(def); got != "" {
			t.Fatalf("InstanceMgmtEndpoint(%q) = %q, want \"\" (it is the %v default state dir)", def, got, m)
		}
		// Trailing separators and redundant elements must not defeat it.
		if got := InstanceMgmtEndpoint(def + string(filepath.Separator)); got != "" {
			t.Fatalf("InstanceMgmtEndpoint(%q + sep) = %q, want \"\"", def, got)
		}
	}
	if got := InstanceMgmtEndpoint(""); got != "" {
		t.Fatalf("InstanceMgmtEndpoint(\"\") = %q, want \"\"", got)
	}
}

func TestInstanceMgmtEndpoint_NonDefaultIsDistinct(t *testing.T) {
	a := InstanceMgmtEndpoint(filepath.FromSlash("/tmp/waired-inst-a"))
	b := InstanceMgmtEndpoint(filepath.FromSlash("/tmp/waired-inst-b"))
	if a == "" || b == "" {
		t.Fatalf("non-default state dirs produced no instance endpoint: a=%q b=%q", a, b)
	}
	if a == b {
		t.Fatalf("two different state dirs share endpoint %q", a)
	}
	// Stable across calls — daemon and client derive it independently.
	if again := InstanceMgmtEndpoint(filepath.FromSlash("/tmp/waired-inst-a")); again != a {
		t.Fatalf("InstanceMgmtEndpoint not deterministic: %q then %q", a, again)
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(a, `\\.\pipe\waired-mgmt-`) {
			t.Fatalf("windows instance endpoint = %q, want a \\\\.\\pipe\\waired-mgmt-* name", a)
		}
		return
	}
	if want := filepath.Join(filepath.FromSlash("/tmp/waired-inst-a"), "mgmt.sock"); a != want {
		t.Fatalf("unix instance endpoint = %q, want %q (beside the state dir)", a, want)
	}
}

// TestMgmtEndpointFor_Precedence pins the documented resolution order.
func TestMgmtEndpointFor_Precedence(t *testing.T) {
	inst := filepath.FromSlash("/tmp/waired-precedence")

	t.Run("non-default state dir beats the mode default", func(t *testing.T) {
		t.Setenv(MgmtSocketEnvOverride, "")
		got := MgmtEndpointFor(System, inst)
		if want := InstanceMgmtEndpoint(inst); got != want {
			t.Fatalf("MgmtEndpointFor(System, %q) = %q, want %q", inst, got, want)
		}
	})

	t.Run("default state dir keeps the mode default", func(t *testing.T) {
		t.Setenv(MgmtSocketEnvOverride, "")
		def := osStateDir(System)
		if got, want := MgmtEndpointFor(System, def), osMgmtEndpoint(System, ""); got != want {
			t.Fatalf("MgmtEndpointFor(System, %q) = %q, want the unchanged default %q", def, got, want)
		}
	})

	t.Run("socket env override beats everything", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("$WAIRED_MGMT_SOCKET does not apply on Windows")
		}
		const forced = "/tmp/forced-precedence.sock"
		t.Setenv(MgmtSocketEnvOverride, forced)
		if got := MgmtEndpointFor(System, inst); got != forced {
			t.Fatalf("MgmtEndpointFor = %q, want %q ($%s wins)", got, forced, MgmtSocketEnvOverride)
		}
	})
}

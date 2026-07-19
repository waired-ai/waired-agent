package paths

import (
	"path/filepath"
	"runtime"
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

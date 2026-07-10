package paths

import (
	"path/filepath"
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

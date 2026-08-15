package controlurl

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolve(t *testing.T) {
	cases := []struct {
		name            string
		explicit        string
		platformDefault string
		want            string
	}{
		{"explicit wins over everything", "https://flag.example.com", "https://envfile.example.com", "https://flag.example.com"},
		{"agent.env wins over baked default", "", "https://envfile.example.com", "https://envfile.example.com"},
		{"baked production default last resort", "", "", Default},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Resolve(c.explicit, c.platformDefault); got != c.want {
				t.Errorf("Resolve(%q, %q) = %q, want %q", c.explicit, c.platformDefault, got, c.want)
			}
		})
	}
}

func TestDefaultConstant(t *testing.T) {
	// The baked default must itself survive normalization (so the
	// last-resort path can't produce a URL the enroll POST rejects).
	got, err := Normalize(Default)
	if err != nil {
		t.Fatalf("Default %q does not normalize: %v", Default, err)
	}
	if got != Default {
		t.Errorf("Default %q normalizes to %q; keep it already-canonical", Default, got)
	}
}

// TestEnvFileDir answers "which agent.env does this OS use" on every
// runner, replacing three //go:build-gated platformDefaultControlURL
// functions that could each only be exercised on their own OS. That is
// the untagged (GOOS, facts) -> plan shape CLAUDE.md §Test discipline
// makes the default, and which #261 names as the primary strategy its
// macOS/Windows unit legs are the catch-basin for. Product contract: the
// directory per OS is fixed by what the installers write.
func TestEnvFileDir(t *testing.T) {
	const stateDir = "/state/dir"
	cases := []struct {
		goos string
		want string
	}{
		// Linux's file is the systemd EnvironmentFile, NOT the state
		// dir: the unit names a fixed path (EnvironmentFile=-/etc/waired/agent.env).
		{"linux", "/etc/waired"},
		// macOS and Windows have no service-manager env file at all, so
		// the installers park it in the system state dir alongside
		// identity.json.
		{"darwin", stateDir},
		{"windows", stateDir},
	}
	for _, c := range cases {
		t.Run(c.goos, func(t *testing.T) {
			if got := envFileDir(c.goos, stateDir); got != c.want {
				t.Errorf("envFileDir(%q, %q) = %q, want %q", c.goos, stateDir, got, c.want)
			}
		})
	}
}

func TestEnvFilePathIsAgentEnv(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		got := EnvFilePath(goos, "/state/dir")
		if filepath.Base(got) != "agent.env" {
			t.Errorf("EnvFilePath(%q, ...) = %q; the file must be named agent.env", goos, got)
		}
	}
}

// TestEnvFilePathReadRoundTrip exercises the whole composition
// (EnvFilePath -> ParseEnvFile) against a real file on any runner, which
// PlatformDefault itself cannot do here: it consults the host's real
// system state dir.
func TestEnvFilePathReadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := EnvFilePath("windows", dir)

	if got := ParseEnvFile(path); got != "" {
		t.Fatalf("no agent.env should yield %q, got %q", "", got)
	}
	if err := os.WriteFile(path, []byte("WAIRED_CONTROL_URL=https://cp.example.test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := ParseEnvFile(path), "https://cp.example.test"; got != want {
		t.Errorf("ParseEnvFile(%q) = %q, want %q", path, got, want)
	}
}

func TestParseEnvFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"plain", "WAIRED_CONTROL_URL=https://cp.example.com\n", "https://cp.example.com"},
		{"double-quoted", "WAIRED_CONTROL_URL=\"https://cp.example.com\"\n", "https://cp.example.com"},
		{"single-quoted", "WAIRED_CONTROL_URL='https://cp.example.com'\n", "https://cp.example.com"},
		{"export prefix", "export WAIRED_CONTROL_URL=https://cp.example.com\n", "https://cp.example.com"},
		{"surrounded by comments/blanks", "# comment\n\nWAIRED_CONTROL_URL=https://cp.example.com\n# trailing\n", "https://cp.example.com"},
		{"commented out", "# WAIRED_CONTROL_URL=https://cp.example.com\n", ""},
		{"other keys only", "FOO=bar\nWAIRED_NO_TRAY=1\n", ""},
		{"empty value", "WAIRED_CONTROL_URL=\n", ""},
		{"whitespace around", "  WAIRED_CONTROL_URL = https://cp.example.com \n", "https://cp.example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "agent.env")
			if err := os.WriteFile(p, []byte(c.content), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := ParseEnvFile(p); got != c.want {
				t.Errorf("ParseEnvFile(%q) = %q, want %q", c.content, got, c.want)
			}
		})
	}
}

func TestParseEnvFile_Missing(t *testing.T) {
	if got := ParseEnvFile(filepath.Join(t.TempDir(), "nope.env")); got != "" {
		t.Errorf("missing file should yield \"\", got %q", got)
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"  ", "", false},
		{"dev.waired.net", "https://dev.waired.net", false},
		{"dev.waired.net/", "https://dev.waired.net", false},
		{"  dev.waired.net  ", "https://dev.waired.net", false},
		{"https://cp.example.com/", "https://cp.example.com", false},
		{"http://cp.example.com", "http://cp.example.com", false},
		{"127.0.0.1:9477", "http://127.0.0.1:9477", false},
		{"localhost:9477", "http://localhost:9477", false},
		{"localhost", "http://localhost", false},
		{"[::1]:9477", "http://[::1]:9477", false},
		{"ftp://cp.example.com", "", true},
		{"https://", "", true},
	}
	for _, c := range cases {
		got, err := Normalize(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("Normalize(%q) = %q, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Normalize(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// PRODUCT CONTRACT — waired-agent#800. The three precedence layers mean
// different things to a person, and the URL alone cannot tell them apart:
// a host that LOST its agent.env resolves to the built-in production URL
// by exactly the same path as a stock install that never had one.
//
// Naming the source is what lets the caller say so. Whether it does is the
// caller's business; that this package can answer is the contract.
func TestResolveWithSource(t *testing.T) {
	for _, tc := range []struct {
		name       string
		explicit   string
		platform   string
		wantURL    string
		wantSource Source
	}{
		{"operator wins", "https://a.example", "https://b.example", "https://a.example", SourceOperator},
		{"installer next", "", "https://b.example", "https://b.example", SourceInstaller},
		{"builtin last", "", "", Default, SourceBuiltin},
		// The #800 shape: agent.env is gone, so platformDefault is "" for
		// the same reason it is "" on a machine that never had one.
		{"lost agent.env is indistinguishable from never having one", "", "", Default, SourceBuiltin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url, src := ResolveWithSource(tc.explicit, tc.platform)
			if url != tc.wantURL || src != tc.wantSource {
				t.Fatalf("ResolveWithSource(%q, %q) = (%q, %q), want (%q, %q)",
					tc.explicit, tc.platform, url, src, tc.wantURL, tc.wantSource)
			}
			if got := Resolve(tc.explicit, tc.platform); got != url {
				t.Errorf("Resolve returned %q but ResolveWithSource returned %q", got, url)
			}
		})
	}
}

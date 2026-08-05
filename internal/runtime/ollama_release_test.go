package runtime

import (
	"path/filepath"
	"strings"
	"testing"
)

// The release table is a pure function of (goos, goarch) precisely so all
// three OSes are checkable from one runner. The facts below were read off
// the real ollama v0.31.1 release, not inferred: macOS publishes ONE
// universal asset and no ROCm overlay, and only Linux's archive already
// carries a bin/ directory.
func TestOllamaReleaseFor(t *testing.T) {
	tests := []struct {
		name       string
		goos       string
		goarch     string
		base       string
		rocm       string
		extractSub string
	}{
		{"linux amd64", "linux", "amd64",
			"ollama-linux-amd64.tar.zst", "ollama-linux-amd64-rocm.tar.zst", ""},
		{"linux arm64", "linux", "arm64",
			"ollama-linux-arm64.tar.zst", "ollama-linux-arm64-rocm.tar.zst", ""},
		{"darwin arm64", "darwin", "arm64",
			"ollama-darwin.tgz", "", "bin"},
		{"darwin amd64 gets the same universal asset", "darwin", "amd64",
			"ollama-darwin.tgz", "", "bin"},
		{"windows amd64", "windows", "amd64",
			"ollama-windows-amd64.zip", "ollama-windows-amd64-rocm.zip", "bin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ollamaReleaseFor(tt.goos, tt.goarch)
			if err != nil {
				t.Fatalf("ollamaReleaseFor(%s, %s): %v", tt.goos, tt.goarch, err)
			}
			if got.Base != tt.base {
				t.Errorf("Base = %q, want %q", got.Base, tt.base)
			}
			if got.ROCm != tt.rocm {
				t.Errorf("ROCm = %q, want %q", got.ROCm, tt.rocm)
			}
			if got.ExtractSub != tt.extractSub {
				t.Errorf("ExtractSub = %q, want %q", got.ExtractSub, tt.extractSub)
			}
		})
	}
}

// An architecture we have no asset for must be an error, not a URL that
// 404s halfway through a first-run install. windows/arm64 is the live case:
// upstream publishes one, but waired ships no windows/arm64 binary to run
// beside it (Makefile verify-cross), so the pairing is untested and refused.
func TestOllamaReleaseFor_Unsupported(t *testing.T) {
	for _, tt := range []struct{ goos, goarch string }{
		{"linux", "386"},
		{"linux", "riscv64"},
		{"windows", "arm64"},
		{"freebsd", "amd64"},
		{"plan9", "amd64"},
	} {
		if _, err := ollamaReleaseFor(tt.goos, tt.goarch); err == nil {
			t.Errorf("ollamaReleaseFor(%s, %s) = nil error, want a refusal", tt.goos, tt.goarch)
		}
	}
}

// One path rule on every OS: the daemon's resolver, the CLI's install
// decision and the installer all join from here, so they cannot drift the
// way #179's "on disk but not on PATH" pair did.
func TestBundledOllamaBinaryPath(t *testing.T) {
	tests := []struct {
		goos     string
		stateDir string
		want     []string
	}{
		{"linux", "/var/lib/waired",
			[]string{"/var/lib/waired", "runtimes", "ollama", "bin", "ollama"}},
		{"darwin", "/Library/Application Support/waired",
			[]string{"/Library/Application Support/waired", "runtimes", "ollama", "bin", "ollama"}},
		{"windows", `C:\ProgramData\waired`,
			[]string{`C:\ProgramData\waired`, "runtimes", "ollama", "bin", "ollama.exe"}},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			want := filepath.Join(tt.want...)
			if got := BundledOllamaBinaryPath(tt.goos, tt.stateDir); got != want {
				t.Errorf("BundledOllamaBinaryPath = %q, want %q", got, want)
			}
			dir := BundledOllamaDir(tt.stateDir)
			if !strings.HasPrefix(BundledOllamaBinaryPath(tt.goos, tt.stateDir), dir) {
				t.Errorf("binary %q is not under the engine dir %q",
					BundledOllamaBinaryPath(tt.goos, tt.stateDir), dir)
			}
		})
	}
}

// Only Windows names an executable with a suffix; getting this wrong is a
// resolver that stats a path nothing ever creates.
func TestOllamaBinaryName(t *testing.T) {
	if got := OllamaBinaryName("windows"); got != "ollama.exe" {
		t.Errorf("windows = %q, want ollama.exe", got)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if got := OllamaBinaryName(goos); got != "ollama" {
			t.Errorf("%s = %q, want ollama", goos, got)
		}
	}
}

func TestParseSHA256Sums(t *testing.T) {
	const body = "abc  ./too-short.zip\n" +
		"0000000000000000000000000000000000000000000000000000000000000001  ./a.tgz\n" +
		"0000000000000000000000000000000000000000000000000000000000000002  b.zip\n" +
		"0000000000000000000000000000000000000000000000000000000000000003 *c.bin\n" +
		"garbage\n\n"
	sums := parseSHA256Sums([]byte(body))
	want := map[string]string{
		"a.tgz": "0000000000000000000000000000000000000000000000000000000000000001",
		"b.zip": "0000000000000000000000000000000000000000000000000000000000000002",
		"c.bin": "0000000000000000000000000000000000000000000000000000000000000003",
	}
	if len(sums) != len(want) {
		t.Fatalf("parsed %v, want %v", sums, want)
	}
	for name, hash := range want {
		if sums[name] != hash {
			t.Errorf("%s = %q, want %q", name, sums[name], hash)
		}
	}
}

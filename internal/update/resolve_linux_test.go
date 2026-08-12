//go:build linux

package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAptCandidate(t *testing.T) {
	tests := []struct {
		name   string
		policy string
		want   string
	}{
		{
			name: "installed and candidate",
			policy: "waired:\n" +
				"  Installed: 1.2.3\n" +
				"  Candidate: 1.3.0\n" +
				"  Version table:\n",
			want: "1.3.0",
		},
		{
			name:   "candidate none",
			policy: "waired:\n  Installed: (none)\n  Candidate: (none)\n",
			want:   "",
		},
		{name: "empty", policy: "", want: ""},
		{name: "unrelated", policy: "N: Unable to locate package waired\n", want: ""},
		{name: "debian revision", policy: "  Candidate: 1.3.0-1\n", want: "1.3.0-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := aptCandidate(tt.policy); got != tt.want {
				t.Errorf("aptCandidate() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveLatest_AptPath(t *testing.T) {
	root := t.TempDir()
	writeSourceList(t, root, "waired-edge.list", "waired-dev-apt-edge")
	refreshed := writeIndex(t, root, "asia-northeast1-apt.pkg.dev_projects_waired-dev_dists_waired-dev-apt-edge_InRelease")

	r := &Resolver{
		aptRoot: root,
		runCommand: func(_ context.Context, name string, args ...string) (string, error) {
			if name != "apt-cache" {
				t.Fatalf("unexpected command %q %v", name, args)
			}
			return "waired:\n  Installed: 1.2.3\n  Candidate: 1.5.0\n", nil
		},
	}
	var res Result
	if err := r.resolveLatest(context.Background(), &res); err != nil {
		t.Fatalf("resolveLatest: %v", err)
	}
	if res.Latest != "1.5.0" {
		t.Errorf("Latest = %q, want 1.5.0", res.Latest)
	}
	if res.LatestSource != SourceAPT {
		t.Errorf("LatestSource = %q, want %q", res.LatestSource, SourceAPT)
	}
	// The whole point of #726: an apt answer must carry how old it is.
	if !res.IndexRefreshedAt.Equal(refreshed) {
		t.Errorf("IndexRefreshedAt = %v, want %v", res.IndexRefreshedAt, refreshed)
	}
}

func TestResolveLatest_GitHubFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0"}`))
	}))
	defer srv.Close()
	root := t.TempDir()
	// Even with an index on disk, a GitHub answer is live and must not be
	// dressed up with someone else's timestamp.
	writeSourceList(t, root, "waired.list", "waired-dev-apt")
	writeIndex(t, root, "host_dists_waired-dev-apt_InRelease")

	r := &Resolver{
		aptRoot:    root,
		apiBase:    srv.URL,
		HTTPClient: srv.Client(),
		runCommand: func(_ context.Context, _ string, _ ...string) (string, error) {
			return "", errors.New("apt-cache: not found") // non-apt host
		},
	}
	var res Result
	if err := r.resolveLatest(context.Background(), &res); err != nil {
		t.Fatalf("resolveLatest: %v", err)
	}
	if res.Latest != "v2.0.0" {
		t.Errorf("Latest = %q, want v2.0.0 (GitHub fallback)", res.Latest)
	}
	if res.LatestSource != SourceGitHub {
		t.Errorf("LatestSource = %q, want %q", res.LatestSource, SourceGitHub)
	}
	if !res.IndexRefreshedAt.IsZero() {
		t.Errorf("IndexRefreshedAt = %v, want zero for a live answer", res.IndexRefreshedAt)
	}
}

func TestAptSuite(t *testing.T) {
	tests := []struct {
		name string
		list string
		want string
	}{
		{
			// The shape linux_apt_ensure_repo actually writes: the options
			// block holds spaces, so a naive field split reads "arch=amd64]"
			// as the suite.
			name: "options block is not counted as fields",
			list: "deb [signed-by=/etc/apt/keyrings/waired-archive-keyring.gpg arch=amd64] " +
				"https://asia-northeast1-apt.pkg.dev/projects/waired-dev waired-dev-apt-edge main\n",
			want: "waired-dev-apt-edge",
		},
		{
			name: "no options block",
			list: "deb https://example.test/repo waired-dev-apt main\n",
			want: "waired-dev-apt",
		},
		{
			name: "comments and blanks are skipped",
			list: "# managed by install.sh\n\ndeb [arch=arm64] https://example.test/r stable main\n",
			want: "stable",
		},
		{name: "deb-src is not a binary source", list: "deb-src https://example.test/r stable main\n", want: ""},
		{name: "unterminated options block", list: "deb [arch=amd64 https://example.test/r stable main\n", want: ""},
		{name: "truncated line", list: "deb https://example.test/r\n", want: ""},
		{name: "empty", list: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := aptSuite(tt.list); got != tt.want {
				t.Errorf("aptSuite() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAptIndexRefreshedAt(t *testing.T) {
	t.Run("edge source list wins and finds its index", func(t *testing.T) {
		root := t.TempDir()
		writeSourceList(t, root, "waired-edge.list", "waired-dev-apt-edge")
		want := writeIndex(t, root, "h_dists_waired-dev-apt-edge_InRelease")
		if got := (&Resolver{aptRoot: root}).aptIndexRefreshedAt(); !got.Equal(want) {
			t.Errorf("aptIndexRefreshedAt = %v, want %v", got, want)
		}
	})

	t.Run("stable source list", func(t *testing.T) {
		root := t.TempDir()
		writeSourceList(t, root, "waired.list", "waired-dev-apt")
		want := writeIndex(t, root, "h_dists_waired-dev-apt_InRelease")
		if got := (&Resolver{aptRoot: root}).aptIndexRefreshedAt(); !got.Equal(want) {
			t.Errorf("aptIndexRefreshedAt = %v, want %v", got, want)
		}
	})

	t.Run("falls back to the unsigned Release form", func(t *testing.T) {
		root := t.TempDir()
		writeSourceList(t, root, "waired.list", "waired-dev-apt")
		want := writeIndex(t, root, "h_dists_waired-dev-apt_Release")
		if got := (&Resolver{aptRoot: root}).aptIndexRefreshedAt(); !got.Equal(want) {
			t.Errorf("aptIndexRefreshedAt = %v, want %v", got, want)
		}
	})

	t.Run("another suite's index is not ours", func(t *testing.T) {
		root := t.TempDir()
		writeSourceList(t, root, "waired.list", "waired-dev-apt")
		// A host tracking stable still has the distro's own indexes, and
		// (after a channel switch) possibly the edge one. Neither answers
		// for the suite the candidate came from.
		writeIndex(t, root, "h_dists_noble_InRelease")
		writeIndex(t, root, "h_dists_waired-dev-apt-edge_InRelease")
		if got := (&Resolver{aptRoot: root}).aptIndexRefreshedAt(); !got.IsZero() {
			t.Errorf("aptIndexRefreshedAt = %v, want zero", got)
		}
	})

	t.Run("no source list", func(t *testing.T) {
		root := t.TempDir()
		writeIndex(t, root, "h_dists_waired-dev-apt_InRelease")
		if got := (&Resolver{aptRoot: root}).aptIndexRefreshedAt(); !got.IsZero() {
			t.Errorf("aptIndexRefreshedAt = %v, want zero without a source list", got)
		}
	})

	t.Run("source list but no index on disk", func(t *testing.T) {
		root := t.TempDir()
		writeSourceList(t, root, "waired.list", "waired-dev-apt")
		if got := (&Resolver{aptRoot: root}).aptIndexRefreshedAt(); !got.IsZero() {
			t.Errorf("aptIndexRefreshedAt = %v, want zero with no index", got)
		}
	})
}

// writeSourceList writes the source line install.sh's linux_apt_ensure_repo
// would write for suite, under root.
func writeSourceList(t *testing.T, root, name, suite string) {
	t.Helper()
	dir := filepath.Join(root, "etc/apt/sources.list.d")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := "deb [signed-by=/etc/apt/keyrings/waired-archive-keyring.gpg arch=amd64] " +
		"https://asia-northeast1-apt.pkg.dev/projects/waired-dev " + suite + " main\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeIndex creates a downloaded-index file with a known mtime and returns
// it. The mtime is set explicitly rather than left at "now" so an equality
// assertion means the code read THIS file, not merely something recent.
func writeIndex(t *testing.T, root, name string) time.Time {
	t.Helper()
	dir := filepath.Join(root, "var/lib/apt/lists")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("Origin: waired\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 8, 9, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAvailable(t *testing.T) {
	tests := []struct {
		name    string
		current string
		latest  string
		want    bool
	}{
		{"newer patch", "1.2.3", "1.2.4", true},
		{"newer minor", "1.2.3", "1.3.0", true},
		{"newer major double-digit", "1.9.0", "1.10.0", true},
		{"equal", "1.2.3", "1.2.3", false},
		{"older latest", "1.3.0", "1.2.9", false},
		{"v-prefixed tag", "1.2.3", "v1.2.4", true},
		{"equal with v", "1.2.3", "v1.2.3", false},
		{"dev build never nags", "0.0.0-dev", "1.2.3", false},
		{"edge build never nags", "0.0.0-abc1234", "1.2.3", false},
		{"edge semver never nags", "0.0.1-edge.20260706123153+5256ed48", "0.0.1-rc15", false},
		{"edge deb ~edge never nags", "0.0.1~edge.20260706123153+5256ed48", "0.0.1-rc15", false},
		{"empty current", "", "1.2.3", false},
		{"empty latest", "1.2.3", "", false},
		{"unparseable latest", "1.2.3", "garbage", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := available(tt.current, tt.latest); got != tt.want {
				t.Errorf("available(%q, %q) = %v, want %v", tt.current, tt.latest, got, tt.want)
			}
		})
	}
}

func TestIsDevVersion(t *testing.T) {
	dev := []string{
		"", "  ", "dev", "0.0.0-dev", "0.0.0-abc1234", "0.0.0",
		// Edge (latest-main) builds: <stable-core>-edge.<ts>+<sha>. These
		// must be treated as non-release so an edge host is never nagged to
		// "update" to a stable tag (the base may even equal that tag).
		"0.0.1-edge.20260610143000+abc1234",
		"1.4.2-edge.20260610143000+deadbee",
		// The dpkg package-version shape uses a tilde ("~edge.") so it sorts
		// below the stable it is based on; it must be recognized too.
		"0.0.1~edge.20260706123153+5256ed48",
		"1.4.2~edge.20260610143000+deadbee",
	}
	for _, v := range dev {
		if !isDevVersion(v) {
			t.Errorf("isDevVersion(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"1.2.3", "v1.0.0", "0.1.0", "1.4.2-rc1"} {
		if isDevVersion(v) {
			t.Errorf("isDevVersion(%q) = true, want false", v)
		}
	}
}

func TestLatestFromGitHub(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"tag_name":"v1.4.2","name":"v1.4.2"}`))
	}))
	defer srv.Close()

	r := &Resolver{apiBase: srv.URL, Repo: "waired-ai/waired-agent", HTTPClient: srv.Client()}
	got, err := r.latestFromGitHub(context.Background())
	if err != nil {
		t.Fatalf("latestFromGitHub: %v", err)
	}
	if got != "v1.4.2" {
		t.Errorf("tag = %q, want v1.4.2", got)
	}
	if want := "/repos/waired-ai/waired-agent/releases/latest"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestLatestFromGitHub_Errors(t *testing.T) {
	t.Run("non-2xx", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden) // rate limited
		}))
		defer srv.Close()
		r := &Resolver{apiBase: srv.URL, HTTPClient: srv.Client()}
		if _, err := r.latestFromGitHub(context.Background()); err == nil {
			t.Fatal("expected error on 403, got nil")
		}
	})
	t.Run("empty tag", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"tag_name":""}`))
		}))
		defer srv.Close()
		r := &Resolver{apiBase: srv.URL, HTTPClient: srv.Client()}
		if _, err := r.latestFromGitHub(context.Background()); err == nil {
			t.Fatal("expected error on empty tag_name, got nil")
		}
	})
}

func TestRepoResolution(t *testing.T) {
	if got := (&Resolver{Repo: "x/y"}).repo(); got != "x/y" {
		t.Errorf("explicit repo = %q, want x/y", got)
	}
	t.Setenv("WAIRED_INSTALL_REPO", "env/repo")
	if got := (&Resolver{}).repo(); got != "env/repo" {
		t.Errorf("env repo = %q, want env/repo", got)
	}
	t.Setenv("WAIRED_INSTALL_REPO", "")
	if got := (&Resolver{}).repo(); got != defaultInstallRepo {
		t.Errorf("default repo = %q, want %q", got, defaultInstallRepo)
	}
}

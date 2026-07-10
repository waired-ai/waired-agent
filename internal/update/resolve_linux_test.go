//go:build linux

package update

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestLatestVersion_AptPath(t *testing.T) {
	r := &Resolver{
		runCommand: func(_ context.Context, name string, args ...string) (string, error) {
			if name != "apt-cache" {
				t.Fatalf("unexpected command %q %v", name, args)
			}
			return "waired:\n  Installed: 1.2.3\n  Candidate: 1.5.0\n", nil
		},
	}
	got, err := r.LatestVersion(context.Background())
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if got != "1.5.0" {
		t.Errorf("LatestVersion = %q, want 1.5.0", got)
	}
}

func TestLatestVersion_GitHubFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0"}`))
	}))
	defer srv.Close()
	r := &Resolver{
		apiBase:    srv.URL,
		HTTPClient: srv.Client(),
		runCommand: func(_ context.Context, _ string, _ ...string) (string, error) {
			return "", errors.New("apt-cache: not found") // non-apt host
		},
	}
	got, err := r.LatestVersion(context.Background())
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if got != "v2.0.0" {
		t.Errorf("LatestVersion = %q, want v2.0.0 (GitHub fallback)", got)
	}
}

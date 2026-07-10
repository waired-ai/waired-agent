package controlclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRevoke(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"ok", http.StatusOK, false},
		{"already-revoked-401-is-success", http.StatusUnauthorized, false},
		{"server-error-surfaces", http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotAuth, gotMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotMethod = r.Method
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			cli := NewWithBearer(srv.URL, func() string { return "tok123" })
			err := cli.Revoke(context.Background())
			if tc.wantErr != (err != nil) {
				t.Fatalf("Revoke err=%v wantErr=%v", err, tc.wantErr)
			}
			if gotMethod != http.MethodPost {
				t.Errorf("method=%q want POST", gotMethod)
			}
			if gotPath != "/v1/devices/self/revoke" {
				t.Errorf("path=%q want /v1/devices/self/revoke", gotPath)
			}
			if gotAuth != "Bearer tok123" {
				t.Errorf("auth=%q want Bearer tok123", gotAuth)
			}
		})
	}
}

// TestRevokeUsesCustomAuthHeader confirms Revoke honours the
// X-Waired-Agent-Bearer transport used when the CP sits behind an
// IAM-gated upstream (same contract as Logout).
func TestRevokeUsesCustomAuthHeader(t *testing.T) {
	var gotCustom, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCustom = r.Header.Get("X-Waired-Agent-Bearer")
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cli := NewWithBearer(srv.URL, func() string { return "tok123" })
	cli.UseCustomAuthHeader = true
	if err := cli.Revoke(context.Background()); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if gotCustom != "tok123" {
		t.Errorf("X-Waired-Agent-Bearer=%q want tok123", gotCustom)
	}
	if gotAuth != "" {
		t.Errorf("Authorization=%q want empty when custom header is used", gotAuth)
	}
}

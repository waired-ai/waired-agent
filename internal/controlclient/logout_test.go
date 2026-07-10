package controlclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLogout(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"ok", http.StatusOK, false},
		{"already-deauthed-401-is-success", http.StatusUnauthorized, false},
		{"server-error-surfaces", http.StatusInternalServerError, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			cli := NewWithBearer(srv.URL, func() string { return "tok123" })
			err := cli.Logout(context.Background())
			if tc.wantErr != (err != nil) {
				t.Fatalf("Logout err=%v wantErr=%v", err, tc.wantErr)
			}
			if gotPath != "/v1/devices/self/logout" {
				t.Errorf("path=%q want /v1/devices/self/logout", gotPath)
			}
			if gotAuth != "Bearer tok123" {
				t.Errorf("auth=%q want Bearer tok123", gotAuth)
			}
		})
	}
}

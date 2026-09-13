package controlclient

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDeviceCallsRenderTheEnvelope: logout, revoke and advertise endpoints
// print the control plane's message, type and hint instead of its JSON,
// keep their prefix, print a non-envelope body trimmed, and still treat a
// 401 from logout and revoke as success (waired-agent#1376).
func TestDeviceCallsRenderTheEnvelope(t *testing.T) {
	_, machineKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	calls := []struct {
		name   string
		prefix string
		call   func(*Client) error
		ok401  bool
	}{
		{"logout", "logout: ", func(c *Client) error { return c.Logout(context.Background()) }, true},
		{"revoke", "revoke: ", func(c *Client) error { return c.Revoke(context.Background()) }, true},
		{"advertise endpoints", "controlclient: advertise endpoints ", func(c *Client) error {
			return c.AdvertiseEndpoints(context.Background(), "dev_test", nil, machineKey)
		}, false},
	}
	bodies := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "envelope",
			status: http.StatusForbidden,
			body:   `{"error":{"type":"device_revoked","message":"this device was removed","hint":"run waired init"}}` + "\n",
			want:   "status 403: this device was removed (device_revoked); run waired init",
		},
		{name: "plain text", status: http.StatusBadGateway, body: "Bad Gateway\n", want: "status 502: Bad Gateway"},
	}
	for _, c := range calls {
		for _, b := range bodies {
			t.Run(c.name+"/"+b.name, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(b.status)
					_, _ = w.Write([]byte(b.body))
				}))
				defer srv.Close()
				err := c.call(NewWithBearer(srv.URL, func() string { return "tok" }))
				if err == nil {
					t.Fatal("err = nil, want an error")
				}
				if got := err.Error(); got != c.prefix+b.want {
					t.Errorf("err = %q, want %q", got, c.prefix+b.want)
				}
				if strings.Contains(err.Error(), "{") {
					t.Errorf("err still carries JSON: %q", err)
				}
			})
		}
		if !c.ok401 {
			continue
		}
		t.Run(c.name+"/401 is success", func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"type":"unauthorized","message":"token revoked"}}`))
			}))
			defer srv.Close()
			if err := c.call(NewWithBearer(srv.URL, func() string { return "tok" })); err != nil {
				t.Errorf("401: err = %v, want nil", err)
			}
		})
	}
}

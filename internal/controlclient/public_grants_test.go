package controlclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// All IDs synthetic (public repo).

func TestAcquirePublicGrantsRoundTrip(t *testing.T) {
	var gotPath, gotAuth string
	var gotReq AcquirePublicGrantsRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","grants":[
			{"grant_id":"grant_1","provider_device_id":"dev_p1","provider_pseudonym":"pub-node-b21c","expires_at":"2026-07-19T00:10:00Z","created":true},
			{"grant_id":"grant_2","provider_device_id":"dev_p2","provider_pseudonym":"pub-node-cafe","expires_at":"2026-07-19T00:10:00Z","created":false}]}`))
	}))
	defer srv.Close()

	cli := New(srv.URL, "tok-acquire")
	res, err := cli.AcquirePublicGrants(context.Background(), AcquirePublicGrantsRequest{
		MinQualityTier: 40, ConsentVersion: 1,
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if gotPath != "/v1/public-share/grants/acquire" || gotAuth != "Bearer tok-acquire" {
		t.Errorf("path/auth = %q / %q", gotPath, gotAuth)
	}
	if gotReq.MinQualityTier != 40 || gotReq.ConsentVersion != 1 || gotReq.Want != 0 {
		t.Errorf("request round-trip: %+v", gotReq)
	}
	if len(res.Grants) != 2 || res.Grants[0].GrantID != "grant_1" || !res.Grants[0].Created ||
		res.Grants[1].Created {
		t.Errorf("response: %+v", res)
	}
}

func TestAcquirePublicGrantsTypedErrorsAndEmpty(t *testing.T) {
	status := http.StatusForbidden
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"status":"ok","grants":[]}`))
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"type":"whatever"}}`))
	}))
	defer srv.Close()
	cli := New(srv.URL, "tok")

	if _, err := cli.AcquirePublicGrants(context.Background(), AcquirePublicGrantsRequest{ConsentVersion: 1}); !errors.Is(err, ErrPublicShareNotEligible) {
		t.Fatalf("403: err = %v, want ErrPublicShareNotEligible", err)
	}
	status = http.StatusTooManyRequests
	if _, err := cli.AcquirePublicGrants(context.Background(), AcquirePublicGrantsRequest{ConsentVersion: 1}); !errors.Is(err, ErrPublicShareRateLimited) {
		t.Fatalf("429: err = %v, want ErrPublicShareRateLimited", err)
	}
	status = http.StatusOK
	res, err := cli.AcquirePublicGrants(context.Background(), AcquirePublicGrantsRequest{ConsentVersion: 1})
	if err != nil || len(res.Grants) != 0 {
		t.Fatalf("empty set must be a clean 200: (%+v, %v)", res, err)
	}
}

func TestRenewAndReleasePublicGrants(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string][]string
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		if len(req["grant_ids"]) != 2 {
			t.Errorf("grant_ids = %v", req["grant_ids"])
		}
		switch r.URL.Path {
		case "/v1/public-share/grants/renew":
			// Partial renewal: grant_2 silently dropped.
			_, _ = w.Write([]byte(`{"status":"ok","renewed":["grant_1"],"expires_at":"2026-07-19T00:20:00Z"}`))
		case "/v1/public-share/grants/release":
			_, _ = w.Write([]byte(`{"status":"ok","released":["grant_1","grant_2"]}`))
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	cli := New(srv.URL, "tok")

	renew, err := cli.RenewPublicGrants(context.Background(), []string{"grant_1", "grant_2"})
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if len(renew.Renewed) != 1 || renew.Renewed[0] != "grant_1" || renew.ExpiresAt == "" {
		t.Fatalf("renew response: %+v", renew)
	}
	rel, err := cli.ReleasePublicGrants(context.Background(), []string{"grant_1", "grant_2"})
	if err != nil || len(rel.Released) != 2 {
		t.Fatalf("release: (%+v, %v)", rel, err)
	}
}

func TestPublicGrantsCustomAuthHeader(t *testing.T) {
	var gotCustom, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCustom = r.Header.Get("X-Waired-Agent-Bearer")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	cli := New(srv.URL, "tok-custom")
	cli.UseCustomAuthHeader = true
	if _, err := cli.ReleasePublicGrants(context.Background(), []string{"g"}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if gotCustom != "tok-custom" || gotAuth != "" {
		t.Errorf("custom-auth headers: custom=%q auth=%q", gotCustom, gotAuth)
	}
}

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestObtainSAIDTokenExplicitWins(t *testing.T) {
	got, err := obtainSAIDToken(context.Background(), http.DefaultClient, "http://cp.invalid", "explicit.jwt.token", "", "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "explicit.jwt.token" {
		t.Fatalf("got %q, want explicit token verbatim", got)
	}
}

func TestObtainSAIDTokenNoSourceErrors(t *testing.T) {
	if _, err := obtainSAIDToken(context.Background(), http.DefaultClient, "http://cp.invalid", "", "", ""); err == nil {
		t.Fatal("expected error when neither --oidc-id-token nor --impersonate-sa is given")
	}
}

func TestFetchOIDCAudience(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/login/oidc-grant/audience" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"audience":"my-client-id.apps.googleusercontent.com"}`))
	}))
	defer srv.Close()

	got, err := fetchOIDCAudience(context.Background(), srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "my-client-id.apps.googleusercontent.com" {
		t.Fatalf("got %q", got)
	}
}

func TestOIDCGrantCompleteLoginSuccess(t *testing.T) {
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"authorized"}`))
	}))
	defer srv.Close()

	if err := oidcGrantCompleteLogin(context.Background(), srv.Client(), srv.URL, "ls_123", "tok"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if gotBody["login_session_id"] != "ls_123" || gotBody["id_token"] != "tok" {
		t.Fatalf("unexpected body: %v", gotBody)
	}
}

func TestOIDCGrantCompleteLoginPropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"type":"identity_not_allowlisted"}}`))
	}))
	defer srv.Close()

	if err := oidcGrantCompleteLogin(context.Background(), srv.Client(), srv.URL, "ls_123", "tok"); err == nil {
		t.Fatal("expected error on 403 response")
	}
}

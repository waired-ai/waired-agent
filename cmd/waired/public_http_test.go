package main

import (
	"net/http"
	"testing"
)

func TestParseMgmtError_ErrorCodeShape(t *testing.T) {
	// The shape errorBody() emits: {"error_code":..,"message":..}.
	e := parseMgmtError(http.StatusConflict, []byte(`{"error_code":"consent_required","message":"accept the current warning first"}`))
	if e.StatusCode != http.StatusConflict {
		t.Fatalf("StatusCode = %d, want %d", e.StatusCode, http.StatusConflict)
	}
	if e.Code != "consent_required" {
		t.Errorf("Code = %q, want consent_required", e.Code)
	}
	if e.Message != "accept the current warning first" {
		t.Errorf("Message = %q", e.Message)
	}
	if mgmtErrorCode(e) != "consent_required" {
		t.Errorf("mgmtErrorCode = %q", mgmtErrorCode(e))
	}
	if !isMgmtStatus(e, http.StatusConflict) {
		t.Errorf("isMgmtStatus(409) = false")
	}
	if isMgmtStatus(e, http.StatusNotFound) {
		t.Errorf("isMgmtStatus(404) = true, want false")
	}
}

func TestParseMgmtError_PlainErrorShape(t *testing.T) {
	// The shape the provider share routes emit: {"error":..}.
	e := parseMgmtError(http.StatusBadRequest, []byte(`{"error":"max_clients must be >= 0"}`))
	if e.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want %d", e.StatusCode, http.StatusBadRequest)
	}
	if e.Code != "" {
		t.Errorf("Code = %q, want empty", e.Code)
	}
	if e.Message != "max_clients must be >= 0" {
		t.Errorf("Message = %q", e.Message)
	}
}

func TestParseMgmtError_NonJSONBody(t *testing.T) {
	// http.Error() plain-text bodies (e.g. the 404 for an unconfigured
	// route) must fall back to the raw string, trimmed.
	e := parseMgmtError(http.StatusNotFound, []byte("public use not configured\n"))
	if e.StatusCode != http.StatusNotFound {
		t.Fatalf("StatusCode = %d, want %d", e.StatusCode, http.StatusNotFound)
	}
	if e.Code != "" {
		t.Errorf("Code = %q, want empty", e.Code)
	}
	if e.Message != "public use not configured" {
		t.Errorf("Message = %q, want the trimmed raw body", e.Message)
	}
	if !isMgmtStatus(e, http.StatusNotFound) {
		t.Errorf("isMgmtStatus(404) = false")
	}
}

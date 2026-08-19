package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeUnloader records the call so the failing case is writable.
type fakeUnloader struct {
	calls int
	tag   string
	err   error
}

func (f *fakeUnloader) UnloadServingModel(context.Context) (string, error) {
	f.calls++
	return f.tag, f.err
}

func unloadRequest(t *testing.T, s *Server, method string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, "/waired/v1/inference/model/unload", nil)
	req.RemoteAddr = "127.0.0.1:1"
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestModelUnloadReportsTheTag(t *testing.T) {
	f := &fakeUnloader{tag: "m:q4"}
	s := New(fakeStatus{}, fakePinger{}).WithModelUnloader(f)
	rec := unloadRequest(t, s, http.MethodPost)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got ModelUnloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Unloaded || got.Model != "m:q4" {
		t.Errorf("response = %+v, want unloaded m:q4", got)
	}
	if f.calls != 1 {
		t.Errorf("controller called %d times, want 1", f.calls)
	}
}

// Nothing resident is a success, not an error: the caller wanted the
// memory back and the memory is back (waired-agent#861).
func TestModelUnloadNothingResidentIs200(t *testing.T) {
	s := New(fakeStatus{}, fakePinger{}).WithModelUnloader(&fakeUnloader{tag: ""})
	rec := unloadRequest(t, s, http.MethodPost)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got ModelUnloadResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Unloaded || got.Model != "" {
		t.Errorf("response = %+v, want not-unloaded and empty", got)
	}
}

func TestModelUnloadErrorIs500(t *testing.T) {
	s := New(fakeStatus{}, fakePinger{}).WithModelUnloader(&fakeUnloader{err: errors.New("engine unreachable")})
	if rec := unloadRequest(t, s, http.MethodPost); rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestModelUnloadRejectsGET(t *testing.T) {
	s := New(fakeStatus{}, fakePinger{}).WithModelUnloader(&fakeUnloader{})
	if rec := unloadRequest(t, s, http.MethodGet); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// A host with no unloader wired must 404 rather than 200 with a lie.
func TestModelUnloadMissingControllerIs404(t *testing.T) {
	if rec := unloadRequest(t, New(fakeStatus{}, fakePinger{}), http.MethodPost); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

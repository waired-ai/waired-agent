package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

type fakePublicShareCtl struct {
	mu         sync.Mutex
	current    state.PublicShareState
	res        PublicShareResult
	err        error
	synced     bool
	maxClients int
}

func newFakePublicShareCtl(initial state.PublicShareState) *fakePublicShareCtl {
	return &fakePublicShareCtl{current: initial, synced: true}
}

func (f *fakePublicShareCtl) Enable(ctx context.Context, maxClients int) (PublicShareResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = ctx
	if f.err != nil {
		return PublicShareResult{}, f.err
	}
	f.current = state.PublicShareOn
	f.maxClients = maxClients
	return f.res, nil
}

func (f *fakePublicShareCtl) Disable(ctx context.Context) (PublicShareResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = ctx
	if f.err != nil {
		return PublicShareResult{}, f.err
	}
	f.current = state.PublicShareOff
	return f.res, nil
}

func (f *fakePublicShareCtl) State() (state.PublicShareState, state.PublicShareState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current, f.current
}

func (f *fakePublicShareCtl) Synced() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.synced
}

func publicShareReq(method, path, body string) *http.Request {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	req.RemoteAddr = "127.0.0.1:1"
	return req
}

func TestPublicShareEnableFlipsStateAndForwardsMaxClients(t *testing.T) {
	pc := newFakePublicShareCtl(state.PublicShareOff)
	pc.res = PublicShareResult{CPSynced: true, MaxClients: 2, MeshShareEnabled: true}
	srv := New(fakeStatus{}, fakePinger{}).WithPublicShareControl(pc)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, publicShareReq(http.MethodPost, "/waired/v1/public/share/enable", `{"max_clients":3}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got PublicShareStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "public" || got.DesiredState != "public" {
		t.Fatalf("after enable: %+v", got)
	}
	if got.CPSynced == nil || !*got.CPSynced || got.MaxClients != 2 {
		t.Fatalf("CP echo missing from response: %+v", got)
	}
	if !strings.Contains(got.Note, PublicShareMeshNote) {
		t.Fatalf("mesh auto-enable note missing: %q", got.Note)
	}
	if pc.maxClients != 3 {
		t.Fatalf("controller saw max_clients=%d, want 3", pc.maxClients)
	}
}

func TestPublicShareDisableSurfacesRevokedGrantsAndNote(t *testing.T) {
	pc := newFakePublicShareCtl(state.PublicShareOn)
	pc.res = PublicShareResult{CPSynced: true, RevokedGrants: 2}
	srv := New(fakeStatus{}, fakePinger{}).WithPublicShareControl(pc)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, publicShareReq(http.MethodPost, "/waired/v1/public/share/disable", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got PublicShareStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "not_public" || got.RevokedGrants != 2 {
		t.Fatalf("after disable: %+v", got)
	}
	if !strings.Contains(got.Note, PublicShareDisableNote) {
		t.Fatalf("disable note missing: %q", got.Note)
	}
}

func TestPublicSharePendingNoteWhenNotSynced(t *testing.T) {
	pc := newFakePublicShareCtl(state.PublicShareOff)
	pc.res = PublicShareResult{CPSynced: false}
	srv := New(fakeStatus{}, fakePinger{}).WithPublicShareControl(pc)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, publicShareReq(http.MethodPost, "/waired/v1/public/share/enable", ""))
	var got PublicShareStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Note, PublicSharePendingNote) {
		t.Fatalf("pending note missing when CPSynced=false: %q", got.Note)
	}
}

func TestPublicShareStatusReportsSync(t *testing.T) {
	pc := newFakePublicShareCtl(state.PublicShareOn)
	pc.synced = false
	srv := New(fakeStatus{}, fakePinger{}).WithPublicShareControl(pc)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, publicShareReq(http.MethodGet, "/waired/v1/public/share", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got PublicShareStateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.State != "public" || got.CPSynced == nil || *got.CPSynced {
		t.Fatalf("status: %+v, want state=public cp_synced=false", got)
	}
	if !strings.Contains(got.Note, PublicSharePendingNote) {
		t.Fatalf("pending note missing on unsynced status: %q", got.Note)
	}
}

func TestPublicShareRoutesAbsentWithoutController(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}) // no WithPublicShareControl
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/waired/v1/public/share"},
		{http.MethodPost, "/waired/v1/public/share/enable"},
		{http.MethodPost, "/waired/v1/public/share/disable"},
	} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, publicShareReq(tc.method, tc.path, ""))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s: expected 404 without controller, got %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestPublicShareMethodAndBodyValidation(t *testing.T) {
	pc := newFakePublicShareCtl(state.PublicShareOff)
	srv := New(fakeStatus{}, fakePinger{}).WithPublicShareControl(pc)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, publicShareReq(http.MethodGet, "/waired/v1/public/share/enable", ""))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET enable: expected 405, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, publicShareReq(http.MethodPost, "/waired/v1/public/share/enable", `{"max_clients":-1}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative max_clients: expected 400, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, publicShareReq(http.MethodPost, "/waired/v1/public/share/enable", `not json`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad JSON: expected 400, got %d", rec.Code)
	}
}

func TestPublicShareErrorPropagates(t *testing.T) {
	pc := newFakePublicShareCtl(state.PublicShareOff)
	pc.err = errors.New("mesh share required")
	srv := New(fakeStatus{}, fakePinger{}).WithPublicShareControl(pc)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, publicShareReq(http.MethodPost, "/waired/v1/public/share/enable", ""))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "mesh share required") {
		t.Errorf("error body: %s", rec.Body.String())
	}
}

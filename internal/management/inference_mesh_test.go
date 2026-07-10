package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/proto/signer"
)

type fakeMeshProvider struct {
	snapshot inferencemesh.Snapshot
}

func (f *fakeMeshProvider) Snapshot() inferencemesh.Snapshot { return f.snapshot }

func TestInferenceMeshEndpointReturnsSnapshot(t *testing.T) {
	prov := &fakeMeshProvider{
		snapshot: inferencemesh.Snapshot{
			GeneratedAt:          "2026-05-09T12:00:00Z",
			SelfDeviceID:         "self-id",
			Reachable:            true,
			StalenessThresholdMS: 15000,
			Self: inferencemesh.PeerView{
				DeviceID:   "self-id",
				DeviceName: "alice-mac",
				OverlayIP:  "100.96.0.10",
				InferenceState: &signer.InferenceState{
					Reachable: true,
					Type:      signer.InferenceTypeOllama,
					Endpoint:  "http://127.0.0.1:11434",
					LastCheck: "2026-05-09T12:00:00Z",
				},
			},
			Peers: []inferencemesh.PeerView{
				{
					DeviceID:   "peer-bob",
					DeviceName: "bob-mac",
					OverlayIP:  "100.96.0.11",
					InferenceState: &signer.InferenceState{
						Reachable: true,
						Type:      signer.InferenceTypeOllama,
						Endpoint:  "http://127.0.0.1:11434",
						LastCheck: "2026-05-09T12:00:00Z",
					},
				},
			},
		},
	}
	srv := New(fakeStatus{s: Status{}}, fakePinger{}).WithInferenceMesh(prov)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/mesh", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got inferencemesh.Snapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if got.SelfDeviceID != "self-id" || !got.Reachable {
		t.Fatalf("snapshot did not round-trip: %+v", got)
	}
	if len(got.Peers) != 1 || got.Peers[0].DeviceID != "peer-bob" {
		t.Fatalf("peers did not round-trip: %+v", got.Peers)
	}
	if got.Self.InferenceState == nil || got.Self.InferenceState.Type != signer.InferenceTypeOllama {
		t.Fatalf("self inference state did not round-trip: %+v", got.Self)
	}
}

func TestInferenceMeshEndpoint404WhenProviderMissing(t *testing.T) {
	// Server with no WithInferenceMesh — the mux must NOT register the
	// route, so a request returns 404.
	srv := New(fakeStatus{s: Status{}}, fakePinger{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/mesh", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestInferenceMeshEndpointRejectsPOST(t *testing.T) {
	srv := New(fakeStatus{s: Status{}}, fakePinger{}).
		WithInferenceMesh(&fakeMeshProvider{snapshot: inferencemesh.Snapshot{}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/mesh", nil)
	req.RemoteAddr = "127.0.0.1:55555"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("code=%d, want 405", rec.Code)
	}
}

func TestInferenceMeshEndpointLoopbackOnly(t *testing.T) {
	srv := New(fakeStatus{s: Status{}}, fakePinger{}).
		WithInferenceMesh(&fakeMeshProvider{snapshot: inferencemesh.Snapshot{}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/mesh", nil)
	req.RemoteAddr = "203.0.113.5:12345" // public IP — loopbackOnly must reject
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d, want 403 (non-loopback)", rec.Code)
	}
}

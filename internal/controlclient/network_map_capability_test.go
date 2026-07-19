package controlclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/proto/signer"
)

// TestSubscribeNetworkMapDeclaresCapabilities pins the §8.4 client
// side: every poll request body declares public-share-v1 so the CP can
// record it on Device.agent_capabilities and (post-B2) emit the Public
// Share map fields to this agent.
func TestSubscribeNetworkMapDeclaresCapabilities(t *testing.T) {
	gotBody := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/network-map/poll" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		gotBody <- body
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":1,"network_id":"wn_test"}` + "\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client(), BearerFn: func() string { return "tok" }}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frames, errs := c.SubscribeNetworkMap(ctx)

	select {
	case nm := <-frames:
		if nm == nil || nm.NetworkID != "wn_test" {
			t.Fatalf("frame = %+v", nm)
		}
	case err := <-errs:
		t.Fatalf("subscribe error: %v", err)
	case <-ctx.Done():
		t.Fatalf("timeout waiting for frame")
	}

	var req struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(<-gotBody, &req); err != nil {
		t.Fatalf("poll body decode: %v", err)
	}
	found := false
	for _, c := range req.Capabilities {
		if c == signer.CapabilityPublicShareV1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("poll body must declare %q, got %q", signer.CapabilityPublicShareV1, req.Capabilities)
	}
}

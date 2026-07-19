package tray

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_InferenceStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/inference/status" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"subsystem_state": "ready",
			"runtimes": {},
			"models": {"ready": ["llama3.2:3b"]},
			"active_endpoints": [],
			"active": {"runtime": "ollama", "model_id": "llama3.2:3b", "variant_id": "q4_K_M"},
			"desired_state": "enabled"
		}`))
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	got, err := c.InferenceStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.SubsystemState != "ready" {
		t.Errorf("subsystem_state: got %q want ready", got.SubsystemState)
	}
	if got.DesiredState != "enabled" {
		t.Errorf("desired_state: got %q want enabled", got.DesiredState)
	}
	if got.Active == nil || got.Active.ModelID != "llama3.2:3b" {
		t.Errorf("active model: got %+v", got.Active)
	}
}

func TestClient_InferenceStatus_404IsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	_, err := c.InferenceStatus(context.Background())
	if !errors.Is(err, ErrInferenceUnsupported) {
		t.Errorf("expected ErrInferenceUnsupported, got %v", err)
	}
}

func TestClient_DisableInference_OK(t *testing.T) {
	got := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	if err := c.DisableInference(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "POST /waired/v1/inference/disable" {
		t.Errorf("server saw %q", got)
	}
}

func TestClient_EnableInference_OK(t *testing.T) {
	got := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	if err := c.EnableInference(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "POST /waired/v1/inference/enable" {
		t.Errorf("server saw %q", got)
	}
}

func TestClient_InferenceToggle_404IsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	if err := c.DisableInference(context.Background()); !errors.Is(err, ErrInferenceUnsupported) {
		t.Errorf("expected ErrInferenceUnsupported (Disable), got %v", err)
	}
	if err := c.EnableInference(context.Background()); !errors.Is(err, ErrInferenceUnsupported) {
		t.Errorf("expected ErrInferenceUnsupported (Enable), got %v", err)
	}
}

func TestClient_StopStartEngine_OK(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	if err := c.StopEngine(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.StartEngine(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /waired/v1/inference/engine/stop", "POST /waired/v1/inference/engine/start"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("server saw %v, want %v", got, want)
	}
}

func TestClient_EngineControl_404IsUnsupported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c := newTestClient(srv.URL)
	if err := c.StopEngine(context.Background()); !errors.Is(err, ErrEngineControlUnsupported) {
		t.Errorf("expected ErrEngineControlUnsupported (Stop), got %v", err)
	}
	if err := c.StartEngine(context.Background()); !errors.Is(err, ErrEngineControlUnsupported) {
		t.Errorf("expected ErrEngineControlUnsupported (Start), got %v", err)
	}
}

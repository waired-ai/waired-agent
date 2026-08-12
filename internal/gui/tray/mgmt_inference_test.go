package tray

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// SetPreferredModel's three answers (waired#808 / waired#812): 202 with
// will_restart telling the tray which path applied, 404 for a daemon
// with no catalog API, and 409 for a host that could not fetch the
// weights — which used to reach the user as a raw transport error.

func TestClient_SetPreferredModel_ReportsHowItApplied(t *testing.T) {
	cases := []struct {
		name            string
		body            string
		wantRestart     bool
		wantDownloading bool
	}{
		{"in-process swap", `{"model_id":"qwen3-8b","will_restart":false}`, false, false},
		{"in-process swap with a pull", `{"model_id":"qwen3-8b","will_restart":false,"downloading":true}`, false, true},
		{"restart fallback", `{"model_id":"qwen3-8b","will_restart":true}`, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method+" "+r.URL.Path != "POST /waired/v1/inference/preferred-model" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(c.body))
			}))
			t.Cleanup(srv.Close)

			got, err := newTestClient(srv.URL).SetPreferredModel(context.Background(), "qwen3-8b")
			if err != nil {
				t.Fatal(err)
			}
			if got.WillRestart != c.wantRestart {
				t.Errorf("WillRestart=%v, want %v", got.WillRestart, c.wantRestart)
			}
			if got.Downloading != c.wantDownloading {
				t.Errorf("Downloading=%v, want %v", got.Downloading, c.wantDownloading)
			}
		})
	}
}

func TestClient_SetPreferredModel_StatusSentinels(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"404 is a daemon without the catalog API", http.StatusNotFound, "", ErrCatalogUnsupported},
		{
			"409 is a host that could not fetch the weights",
			http.StatusConflict,
			`{"error":"model_switch_unavailable","message":"management: this host cannot apply that model switch"}`,
			ErrModelSwitchUnavailable,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			t.Cleanup(srv.Close)

			_, err := newTestClient(srv.URL).SetPreferredModel(context.Background(), "qwen3-8b")
			if !errors.Is(err, c.want) {
				t.Fatalf("err=%v, want %v", err, c.want)
			}
			// The sentinel replaces the transport error rather than
			// wrapping it, so no JSON body can reach a dialog.
			if strings.Contains(err.Error(), "{") || strings.Contains(err.Error(), "HTTP") {
				t.Errorf("err=%q, want no transport detail", err)
			}
		})
	}
}

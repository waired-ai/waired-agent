package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// catalogStub serves a minimal /inference/catalog with the given families
// (and an optional status override) so the pull confirmation gate can be
// exercised without a live agent.
func catalogStub(t *testing.T, status int, familiesJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/inference/catalog" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"engine":"ollama","families":` + familiesJSON + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConfirmModelFitsForPull(t *testing.T) {
	overSpec := `[{"model_id":"qwen3.6-35b-a3b","display_name":"Qwen3.6 35B","fits":false,"deficit_label":"needs 32 GB RAM (have 31 GB)"}]`
	fitsFine := `[{"model_id":"qwen3.5-9b","display_name":"Qwen3.5 9B","fits":true}]`

	t.Run("over-spec with --yes proceeds", func(t *testing.T) {
		srv := catalogStub(t, http.StatusOK, overSpec)
		var out bytes.Buffer
		proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.6-35b-a3b", true, &out, strings.NewReader(""))
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !proceed {
			t.Error("proceed = false, want true (--yes)")
		}
		if !strings.Contains(out.String(), "recommended spec") {
			t.Errorf("warning not shown: %q", out.String())
		}
	})

	t.Run("over-spec non-interactive without --yes aborts", func(t *testing.T) {
		srv := catalogStub(t, http.StatusOK, overSpec)
		var out bytes.Buffer
		// go test runs with a non-TTY stdin, so the interactive prompt is
		// skipped and the gate demands --yes.
		proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.6-35b-a3b", false, &out, strings.NewReader(""))
		if err == nil {
			t.Fatal("err = nil, want a 'use --yes' abort error")
		}
		if proceed {
			t.Error("proceed = true, want false")
		}
		if !strings.Contains(err.Error(), "--yes") {
			t.Errorf("err = %q, want it to mention --yes", err.Error())
		}
	})

	t.Run("fitting model is not gated", func(t *testing.T) {
		srv := catalogStub(t, http.StatusOK, fitsFine)
		var out bytes.Buffer
		proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.5-9b", false, &out, strings.NewReader(""))
		if err != nil || !proceed {
			t.Fatalf("proceed=%v err=%v, want true/nil", proceed, err)
		}
		if out.Len() != 0 {
			t.Errorf("unexpected output for a fitting model: %q", out.String())
		}
	})

	t.Run("catalog 404 fails open", func(t *testing.T) {
		srv := catalogStub(t, http.StatusNotFound, "")
		var out bytes.Buffer
		proceed, err := confirmModelFitsForPull(srv.URL, "anything", false, &out, strings.NewReader(""))
		if err != nil || !proceed {
			t.Fatalf("proceed=%v err=%v, want true/nil (fail-open)", proceed, err)
		}
	})

	t.Run("unmatched model fails open", func(t *testing.T) {
		srv := catalogStub(t, http.StatusOK, overSpec)
		var out bytes.Buffer
		proceed, err := confirmModelFitsForPull(srv.URL, "some-other-model", false, &out, strings.NewReader(""))
		if err != nil || !proceed {
			t.Fatalf("proceed=%v err=%v, want true/nil (fail-open)", proceed, err)
		}
	})
}

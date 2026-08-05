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

	// This pair inverts what it used to assert: --yes let a pull past
	// fits=false, on the reading that the flag skipped a confirmation.
	// Since waired-ai/waired#1056 (2026-08-03) and #464, fits=false means
	// one thing only — this computer does not have the memory, and
	// loading it is a certain OOM. That is the ONE gate the ratified
	// policy allows to refuse, the browser has always hard-disabled the
	// radio for it, and docs-site's choose-a-model.mdx already told users
	// Waired refuses it. #465 item 4 is the parity fix; --yes goes back
	// to its real job, the not-recommended confirmation below.
	t.Run("no memory for it is refused, --yes or not", func(t *testing.T) {
		for _, assumeYes := range []bool{false, true} {
			srv := catalogStub(t, http.StatusOK, overSpec)
			var out bytes.Buffer
			proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.6-35b-a3b", assumeYes, &out, strings.NewReader(""))
			if err == nil {
				t.Fatalf("--yes=%v: err = nil, want a refusal", assumeYes)
			}
			if proceed {
				t.Errorf("--yes=%v: proceed = true, want false", assumeYes)
			}
			// The shortfall is what makes the refusal actionable.
			if !strings.Contains(err.Error(), "needs 32 GB RAM") {
				t.Errorf("--yes=%v: err = %q, want the shortfall named", assumeYes, err.Error())
			}
			if strings.Contains(err.Error(), "--yes") {
				t.Errorf("--yes=%v: err = %q must not offer an escape that does not exist",
					assumeYes, err.Error())
			}
		}
	})

	t.Run("not recommended is a confirmation, not a refusal", func(t *testing.T) {
		notRec := `[{"model_id":"qwen3.6-27b","display_name":"Qwen3.6 27B","fits":true,` +
			`"fit":{"runnable":true,"not_recommended":true,"not_recommended_reason":"weights_spill"}}]`
		srv := catalogStub(t, http.StatusOK, notRec)
		var out bytes.Buffer
		proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.6-27b", false, &out, strings.NewReader(""))
		if err != nil || !proceed {
			t.Fatalf("proceed=%v err=%v, want true/nil — it runs, it is just not the pick", proceed, err)
		}
		if !strings.Contains(out.String(), "would not choose it here") {
			t.Errorf("the demotion was not explained: %q", out.String())
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

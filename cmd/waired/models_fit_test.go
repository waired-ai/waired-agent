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

	// This block inverts what it used to assert, and says so: fits=false
	// was a refusal with no --yes escape (waired-ai/waired#1056,
	// 2026-08-03). The 2026-08-08 owner decision (waired-ai/waired#1067,
	// #583) supersedes that: no surface refuses a model any more. Product
	// contract now pinned: fits=false warns with the shortfall and asks,
	// default No; --yes alone still does not consent (it skips
	// confirmations whose safe answer is yes); --yes --force is the
	// scripted consent; a non-interactive pull without both declines
	// WITHOUT an error — a choice, not a fault.
	t.Run("no memory for it asks; only --yes --force auto-consents", func(t *testing.T) {
		for _, tc := range []struct {
			name             string
			assumeYes, force bool
			wantProceed      bool
		}{
			{"bare decline", false, false, false},
			{"--yes alone declines", true, false, false},
			{"--force alone declines", false, true, false},
			{"--yes --force proceeds", true, true, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv := catalogStub(t, http.StatusOK, overSpec)
				var out bytes.Buffer
				proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.6-35b-a3b", tc.assumeYes, tc.force, &out, strings.NewReader(""))
				if err != nil {
					t.Fatalf("err = %v, want nil — a decline is not a fault", err)
				}
				if proceed != tc.wantProceed {
					t.Errorf("proceed = %v, want %v", proceed, tc.wantProceed)
				}
				// The shortfall is what makes the warning actionable.
				if !strings.Contains(out.String(), "needs 32 GB RAM") {
					t.Errorf("output %q does not name the shortfall", out.String())
				}
				if !tc.wantProceed && !strings.Contains(out.String(), "--yes --force") {
					t.Errorf("a decline must name the consent that exists: %q", out.String())
				}
			})
		}
	})

	// The decision seam, all combinations: auto-consent takes BOTH flags,
	// a present human is asked, an absent one declines
	// (waired-ai/waired#1067, 2026-08-08 owner decision).
	t.Run("unfitPullAction table", func(t *testing.T) {
		for _, tc := range []struct {
			assumeYes, force, interactive bool
			want                          pullFitAction
		}{
			{false, false, false, pullDecline},
			{false, false, true, pullAsk},
			{false, true, false, pullDecline},
			{false, true, true, pullAsk},
			{true, false, false, pullDecline},
			{true, false, true, pullAsk},
			{true, true, false, pullProceed},
			{true, true, true, pullProceed},
		} {
			if got := unfitPullAction(tc.assumeYes, tc.force, tc.interactive); got != tc.want {
				t.Errorf("unfitPullAction(yes=%v force=%v tty=%v) = %v, want %v",
					tc.assumeYes, tc.force, tc.interactive, got, tc.want)
			}
		}
	})

	t.Run("not recommended is a confirmation, not a refusal", func(t *testing.T) {
		notRec := `[{"model_id":"qwen3.6-27b","display_name":"Qwen3.6 27B","fits":true,` +
			`"fit":{"runnable":true,"not_recommended":true,"not_recommended_reason":"weights_spill"}}]`
		srv := catalogStub(t, http.StatusOK, notRec)
		var out bytes.Buffer
		proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.6-27b", false, false, &out, strings.NewReader(""))
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
		proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.5-9b", false, false, &out, strings.NewReader(""))
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
		proceed, err := confirmModelFitsForPull(srv.URL, "anything", false, false, &out, strings.NewReader(""))
		if err != nil || !proceed {
			t.Fatalf("proceed=%v err=%v, want true/nil (fail-open)", proceed, err)
		}
	})

	t.Run("unmatched model fails open", func(t *testing.T) {
		srv := catalogStub(t, http.StatusOK, overSpec)
		var out bytes.Buffer
		proceed, err := confirmModelFitsForPull(srv.URL, "some-other-model", false, false, &out, strings.NewReader(""))
		if err != nil || !proceed {
			t.Fatalf("proceed=%v err=%v, want true/nil (fail-open)", proceed, err)
		}
	})
}

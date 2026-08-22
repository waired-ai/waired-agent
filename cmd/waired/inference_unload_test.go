package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// `waired inference unload` had no test at all, and its rendering is where
// waired-agent#943 was VISIBLE: on a host whose engine has no unload axis the
// daemon's honest refusal has to reach the operator, and the pre-existing arm
// turns any 200-without-Unloaded into "No model was loaded." — which on such a
// host is simply false.
//
// Both cases are here because only the pair says anything. A test of the
// refusal alone would pass just as well if the CLI had stopped printing the
// ordinary answer.

func TestRunInferenceUnload_RendersTheDaemonsRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"this engine has no unload axis: ` +
			`the inference engine on this computer holds the model for as long as the engine runs. ` +
			"To free the memory, stop the engine: `waired inference engine stop`\"}"))
	}))
	defer srv.Close()

	var err error
	out := captureStdout(t, func() { err = runInferenceUnload(srv.URL) })
	if err != nil {
		t.Fatalf("runInferenceUnload = %v, want nil: a host that cannot unload is not a fault", err)
	}
	if strings.Contains(out, "No model was loaded") {
		t.Fatalf("printed the falsehood #943 is about:\n%s", out)
	}
	// The sentence, not the JSON it arrived in. This is the assertion that
	// catches a body shape the CLI cannot unwrap.
	if strings.Contains(out, `{"error"`) || strings.Contains(out, "status 409") {
		t.Fatalf("printed the raw transport rather than the sentence:\n%s", out)
	}
	if !strings.Contains(out, "holds the model for as long as the engine runs") {
		t.Errorf("the reason did not reach the operator:\n%s", out)
	}
	if !strings.Contains(out, "waired inference engine stop") {
		t.Errorf("the refusal did not name the way out (waired#1067):\n%s", out)
	}
}

func TestRunInferenceUnload_OrdinaryAnswersAreUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"nothing resident", `{"unloaded":false}`, "No model was loaded."},
		{"unloaded", `{"unloaded":true,"model":"qwen3:8b-q4_K_M"}`, "Unloaded qwen3:8b-q4_K_M."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			var err error
			out := captureStdout(t, func() { err = runInferenceUnload(srv.URL) })
			if err != nil {
				t.Fatalf("runInferenceUnload = %v", err)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("output = %q, want it to contain %q", out, tc.want)
			}
		})
	}
}

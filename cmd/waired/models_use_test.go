package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// PRODUCT CONTRACT (waired-agent#753): the three answers are three
// different facts about this computer, and an operator has to be able to
// tell which one happened.
//
// The failure this pins is the one the tray hit (#769): silently dropping
// will_restart and downloading answers "switched" for a machine that is
// about to fetch several gigabytes, or that has not switched at all yet —
// while the OLD model keeps answering the whole time.
func TestFormatModelsUse(t *testing.T) {
	tests := []struct {
		name                     string
		willRestart, downloading bool
		want                     string
	}{
		{
			name: "weights already local, applied in process",
			want: "qwen3.5-4b is now the model this computer runs.",
		},
		{
			name:        "a download has to land first, and the old model covers it",
			downloading: true,
			want: "qwen3.5-4b will run on this computer once it finishes downloading.\n" +
				"The current model keeps answering until then.",
		},
		{
			name:        "cross-engine: the switch needs the restart to apply",
			willRestart: true,
			want:        "qwen3.5-4b is recorded as the model this computer runs. The background service restarts to apply it.",
		},
		{
			// A restart that also has to download says the restart part:
			// it is the one that decides when the machine changes, and
			// the download rides along with it.
			name:        "restart wins over the download note",
			willRestart: true,
			downloading: true,
			want:        "qwen3.5-4b is recorded as the model this computer runs. The background service restarts to apply it.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatModelsUse("qwen3.5-4b", tt.willRestart, tt.downloading); got != tt.want {
				t.Errorf("formatModelsUse() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// PRODUCT CONTRACT (waired-agent#257): a refused switch must not read as a
// switch, and must say that the choice was KEPT — the daemon applies it by
// itself once pulls work again, so "nothing happened" would be wrong in
// the one way that costs the operator the most.
//
// All three refusals are 409, so the code is what separates them. A raw
// HTTP error here would destroy the only information the operator needs.
func TestFormatModelsUseError(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		want        string
		wantHandled bool
	}{
		{
			name:        "an unknown name points at the list",
			status:      http.StatusNotFound,
			body:        `{"error_code":"model_not_found","message":"no bundled manifest with that model_id"}`,
			want:        "No model with that name. Run `waired models ls` to see what this computer can run.",
			wantHandled: true,
		},
		{
			// The daemon is the only party that knows the successor, so
			// its sentence is reported rather than rewritten (#200).
			name:        "a retired name is answered with the successor the daemon named",
			status:      http.StatusConflict,
			body:        `{"error_code":"model_retired","message":"\"qwen3.5-4b\" was retired; use \"qwen3.6-27b\" instead"}`,
			want:        `"qwen3.5-4b" was retired; use "qwen3.6-27b" instead`,
			wantHandled: true,
		},
		{
			// waired-ai/waired#1456: --window 1m on a model whose publisher
			// documents no way past its own length. The daemon's sentence
			// carries the model's length, so it is reported rather than
			// rewritten, and the remedy names BOTH ways out — a different
			// model, or the coding window on this one.
			name:   "an unreachable window names the length and both ways out",
			status: http.StatusConflict,
			body: `{"error_code":"window_not_reachable",` +
				`"message":"qwen3.5-0.8b serves up to 262144 tokens; it documents no way past that"}`,
			want: "qwen3.5-0.8b serves up to 262144 tokens; it documents no way past that\n" +
				"Pick a model that documents a longer window, or drop --window.",
			wantHandled: true,
		},
		{
			name:        "an error this build has no reading of is left to the caller",
			status:      http.StatusInternalServerError,
			body:        `{"error_code":"restart_unavailable","message":"no restart mechanism wired"}`,
			wantHandled: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &mgmtStatusError{StatusCode: tt.status, Message: tt.body}
			got, handled := formatModelsUseError("", "qwen3.5-4b", err)
			if handled != tt.wantHandled {
				t.Fatalf("handled = %v, want %v (got %q)", handled, tt.wantHandled, got)
			}
			if handled && got != tt.want {
				t.Errorf("formatModelsUseError() =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}
}

// The weights-unavailable refusal names what this computer is still
// running, which is the difference between "your machine is broken" and
// "your machine is fine and kept doing what it was doing".
func TestFormatModelsUseErrorNamesWhatIsStillServing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/inference/catalog" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(catalogDetailResp{
			Families: []catalogDetailFamily{
				{ModelID: "qwen3.5-4b"},
				{ModelID: "qwen3.5-2b", Active: true},
			},
		})
	}))
	defer srv.Close()

	err := &mgmtStatusError{
		StatusCode: http.StatusConflict,
		Message:    `{"error_code":"model_switch_unavailable","message":"cannot fetch weights"}`,
	}
	got, handled := formatModelsUseError(srv.URL, "qwen3.5-9b", err)
	if !handled {
		t.Fatal("the weights-unavailable refusal was not recognised")
	}
	want := "Couldn't download the weights for qwen3.5-9b, so this computer keeps running qwen3.5-2b.\n" +
		"The choice is recorded and applies once downloads work again."
	if got != want {
		t.Errorf("formatModelsUseError() =\n  %q\nwant\n  %q", got, want)
	}
}

// A daemon that cannot say what it is serving gets the same sentence
// minus the clause, rather than a guess. The "the choice is recorded"
// half is the part that must survive, because it is the part that is
// true regardless.
func TestFormatModelsUseErrorWithoutAServingModel(t *testing.T) {
	err := &mgmtStatusError{
		StatusCode: http.StatusConflict,
		Message:    `{"error_code":"model_switch_unavailable","message":"cannot fetch weights"}`,
	}
	// An unreachable management URL: servingModelID answers "".
	got, handled := formatModelsUseError("http://127.0.0.1:1", "qwen3.5-9b", err)
	if !handled {
		t.Fatal("the weights-unavailable refusal was not recognised")
	}
	want := "Couldn't download the weights for qwen3.5-9b.\n" +
		"The choice is recorded and applies once downloads work again."
	if got != want {
		t.Errorf("formatModelsUseError() =\n  %q\nwant\n  %q", got, want)
	}
}

// The request shape the daemon parses. Pinned because the endpoint reads
// model_id and nothing else, and a renamed field would be accepted as an
// empty body — which the handler reads as the "none" arm's neighbour
// rather than as an error.
func TestModelsUseRequestBody(t *testing.T) {
	var req struct {
		ModelID       string `json:"model_id"`
		ContextWindow int    `json:"context_window"`
	}
	if err := json.Unmarshal(mustMarshalPreferredModel("qwen3.5-4b", 0), &req); err != nil {
		t.Fatalf("the daemon could not parse what we send: %v", err)
	}
	if req.ModelID != "qwen3.5-4b" {
		t.Errorf("model_id = %q, want the model asked for", req.ModelID)
	}
	// The coding window is omitted rather than sent as a number, so a daemon
	// that predates the field reads exactly the body it always read.
	if req.ContextWindow != 0 {
		t.Errorf("context_window = %d, want it absent for the coding window", req.ContextWindow)
	}

	if err := json.Unmarshal(mustMarshalPreferredModel("qwen3.5-4b", hostfit.ServingWindow1M), &req); err != nil {
		t.Fatalf("the long window body does not parse: %v", err)
	}
	if req.ContextWindow != hostfit.ServingWindow1M {
		t.Errorf("context_window = %d, want %d", req.ContextWindow, hostfit.ServingWindow1M)
	}
}

func TestParseWindowFlag(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
		ok   bool
	}{
		// 0, not 200704: the coding window is 0 wherever this value goes,
		// and an omitted field keeps the request byte-identical for a daemon
		// that predates the choice.
		{"", 0, true},
		{"200k", 0, true},
		{"200K", 0, true},
		{"1m", hostfit.ServingWindow1M, true},
		{"1M", hostfit.ServingWindow1M, true},
		{" 1m ", hostfit.ServingWindow1M, true},
		// Not a silent fall back to the coding window: someone who typed a
		// window meant one, and serving the other without saying so is the
		// failure the window contract exists to remove.
		{"1000000", 0, false},
		{"200704", 0, false},
		{"big", 0, false},
	} {
		got, err := parseWindowFlag(tc.in)
		if tc.ok && err != nil {
			t.Errorf("parseWindowFlag(%q): %v", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("parseWindowFlag(%q) accepted it", tc.in)
		}
		if tc.ok && got != tc.want {
			t.Errorf("parseWindowFlag(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

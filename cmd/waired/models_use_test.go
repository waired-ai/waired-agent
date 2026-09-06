package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
// Both refusals are 409, so the code is what separates them. A raw HTTP
// error here would destroy the only information the operator needs.
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
		ModelID string `json:"model_id"`
	}
	if err := json.Unmarshal(mustMarshalPreferredModel("qwen3.5-4b"), &req); err != nil {
		t.Fatalf("the daemon could not parse what we send: %v", err)
	}
	if req.ModelID != "qwen3.5-4b" {
		t.Errorf("model_id = %q, want the model asked for", req.ModelID)
	}
}

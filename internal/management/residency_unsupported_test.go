package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// waired-agent#943: a host whose engine has no residency axis has to say so
// on every route that offers one, rather than answering for another engine.

// unsupportedUnloader is a host whose serving engine has no unload axis.
type unsupportedUnloader struct{}

func (unsupportedUnloader) UnloadServingModel(context.Context) (string, error) {
	return "", fmt.Errorf("%w: the AI engine on this computer holds the model for as long as the engine runs",
		ErrUnloadNotSupported)
}

// TestModelUnload_NotSupportedIs409 pins the status, and pins WHY it is a
// status rather than a field.
//
// A 200 carrying a new "unsupported" flag would be rendered by every shipped
// CLI as "No model was loaded." — the exact falsehood being fixed — because
// that is what they print for a 200 without Unloaded. A 409 reaches those
// clients as an error carrying the daemon's own sentence.
func TestModelUnload_NotSupportedIs409(t *testing.T) {
	srv := New(fakeStatus{}, fakePinger{}).WithModelUnloader(unsupportedUnloader{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/waired/v1/inference/model/unload", nil)
	req.RemoteAddr = "127.0.0.1:1"
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %q)", err, rec.Body.String())
	}
	if body.Error == "" {
		t.Error("the 409 carried no sentence; an older CLI has nothing to print")
	}
}

// TestResidency_SupportedFlagTravels: the tray gates the residency presets
// AND the Unload item on this block, so an engine without the axis has to be
// distinguishable from an older daemon that makes no claim at all.
func TestResidency_SupportedFlagTravels(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unsupported bool
		want        bool
	}{
		{"an engine with the axis", false, true},
		{"an engine without it", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rc := &fakeResidencyCtl{idle: 0, unsupported: tc.unsupported}
			srv := New(fakeStatus{}, fakePinger{}).WithResidencyControl(rc)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/waired/v1/inference/residency", nil)
			req.RemoteAddr = "127.0.0.1:1"
			srv.Handler().ServeHTTP(rec, req)

			var got ResidencyResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Supported == nil {
				t.Fatal("Supported is absent; a client cannot tell this from an older daemon")
			}
			if *got.Supported != tc.want {
				t.Errorf("Supported = %v, want %v", *got.Supported, tc.want)
			}
		})
	}
}

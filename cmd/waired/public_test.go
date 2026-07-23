package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/observabilityclient"
	"github.com/waired-ai/waired-agent/internal/observability"
)

// publicHandlers configures a fake Local Management API for the status
// tests. A nil body with status 0 leaves that route unregistered (any
// GET 404s, exercising the "unsupported by this daemon" path); a non-zero
// status registers a handler emitting the JSON encoding of body.
type publicHandlers struct {
	shareStatus int
	shareBody   any
	useStatus   int
	useBody     any
	// events, when non-nil, is served at the observability events route;
	// nil leaves it 404 (older daemon → no nudge).
	events *observabilityclient.EventsResponse
}

func newPublicServer(t *testing.T, h publicHandlers) string {
	t.Helper()
	mux := http.NewServeMux()
	if h.shareStatus != 0 {
		mux.HandleFunc("/waired/v1/public/share", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(h.shareStatus)
			_ = json.NewEncoder(w).Encode(h.shareBody)
		})
	}
	if h.useStatus != 0 {
		mux.HandleFunc("/waired/v1/public/use", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(h.useStatus)
			_ = json.NewEncoder(w).Encode(h.useBody)
		})
	}
	if h.events != nil {
		mux.HandleFunc("/waired/v1/observability/events", func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(h.events)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func pubBoolPtr(b bool) *bool { return &b }

func TestRunPublicStatus_RendersProviderAndConsumerState(t *testing.T) {
	url := newPublicServer(t, publicHandlers{
		shareStatus: http.StatusOK,
		shareBody: management.PublicShareStateResponse{
			State:        "public",
			DesiredState: "public",
			CPSynced:     pubBoolPtr(true),
			MaxClients:   3,
		},
		useStatus: http.StatusOK,
		useBody: management.PublicUseResponse{
			Mode:           "auto",
			EffectiveMode:  "auto",
			MinQualityTier: 2,
			Main:           true,
			Sub:            false,
			Consented:      true,
			WarningVersion: 1,
		},
	})

	var buf bytes.Buffer
	if err := runPublicStatus(url, false, &buf); err != nil {
		t.Fatalf("runPublicStatus: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Sharing this computer: on",
		"Guest limit: 3 at once",
		"Use public nodes: auto",
		"Consented: yes",
		"Minimum quality tier: 2",
		"Main agent: on",
		"Sub agents: off",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestRunPublicStatus_NotEnrolledEmptyState(t *testing.T) {
	url := newPublicServer(t, publicHandlers{
		shareStatus: http.StatusOK,
		shareBody:   management.PublicShareStateResponse{State: "", CPSynced: pubBoolPtr(true)},
		useStatus:   http.StatusOK,
		useBody:     management.PublicUseResponse{Mode: "off", EffectiveMode: "off", Consented: true},
	})

	var buf bytes.Buffer
	if err := runPublicStatus(url, false, &buf); err != nil {
		t.Fatalf("runPublicStatus: %v", err)
	}
	if !strings.Contains(buf.String(), "not enrolled") {
		t.Errorf("output missing 'not enrolled'\n---\n%s", buf.String())
	}
}

func TestRunPublicStatus_ShareUnsupported404(t *testing.T) {
	url := newPublicServer(t, publicHandlers{
		// share route unregistered -> 404
		useStatus: http.StatusOK,
		useBody:   management.PublicUseResponse{Mode: "off", EffectiveMode: "off", Consented: true},
	})

	var buf bytes.Buffer
	if err := runPublicStatus(url, false, &buf); err != nil {
		t.Fatalf("runPublicStatus returned err, want nil: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Sharing this computer: unsupported by this daemon (upgrade waired-agent)") {
		t.Errorf("missing share-unsupported line\n---\n%s", out)
	}
	// Consumer block must still render.
	if !strings.Contains(out, "Use public nodes: off") {
		t.Errorf("consumer block did not render after share 404\n---\n%s", out)
	}
}

func TestRunPublicStatus_UseUnsupported404(t *testing.T) {
	url := newPublicServer(t, publicHandlers{
		shareStatus: http.StatusOK,
		shareBody:   management.PublicShareStateResponse{State: "not_public", CPSynced: pubBoolPtr(true)},
		// use route unregistered -> 404
	})

	var buf bytes.Buffer
	if err := runPublicStatus(url, false, &buf); err != nil {
		t.Fatalf("runPublicStatus returned err, want nil: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Sharing this computer: off") {
		t.Errorf("provider block did not render before use 404\n---\n%s", out)
	}
	if !strings.Contains(out, "Use public nodes: unsupported by this daemon (upgrade waired-agent)") {
		t.Errorf("missing use-unsupported line\n---\n%s", out)
	}
}

func TestRunPublicStatus_PrintsPendingNoteVerbatim(t *testing.T) {
	const note = management.PublicSharePendingNote
	url := newPublicServer(t, publicHandlers{
		shareStatus: http.StatusOK,
		shareBody: management.PublicShareStateResponse{
			State:    "public",
			CPSynced: pubBoolPtr(false),
			Note:     note,
		},
		useStatus: http.StatusOK,
		useBody:   management.PublicUseResponse{Mode: "off", EffectiveMode: "off", Consented: true},
	})

	var buf bytes.Buffer
	if err := runPublicStatus(url, false, &buf); err != nil {
		t.Fatalf("runPublicStatus: %v", err)
	}
	if !strings.Contains(buf.String(), note) {
		t.Errorf("pending note not printed verbatim\nwant substring: %q\n---\n%s", note, buf.String())
	}
}

const madeUpNudge = "Zephyr-42 sample nudge copy that only the server could have supplied."

func nudgeEvents(reason string) *observabilityclient.EventsResponse {
	return &observabilityclient.EventsResponse{
		Events: []observability.Event{{
			Seq:  1,
			Kind: observability.KindPublicShareNudge,
			PublicShareNudge: &observability.PublicShareNudgeEvent{
				Reason:  reason,
				Message: madeUpNudge,
			},
		}},
	}
}

func TestRunPublicStatus_ShowsNudgeWhenNotConsented(t *testing.T) {
	url := newPublicServer(t, publicHandlers{
		shareStatus: http.StatusOK,
		shareBody:   management.PublicShareStateResponse{State: "not_public", CPSynced: pubBoolPtr(true)},
		useStatus:   http.StatusOK,
		useBody:     management.PublicUseResponse{Mode: "off", EffectiveMode: "off", Consented: false},
		events:      nudgeEvents("no_candidate"),
	})

	var buf bytes.Buffer
	if err := runPublicStatus(url, false, &buf); err != nil {
		t.Fatalf("runPublicStatus: %v", err)
	}
	// The exact server-supplied string must appear — proving the CLI does
	// not hardcode the copy.
	if !strings.Contains(buf.String(), madeUpNudge) {
		t.Errorf("nudge message not printed from server event\n---\n%s", buf.String())
	}
}

func TestRunPublicStatus_NoNudgeWhenConsented(t *testing.T) {
	url := newPublicServer(t, publicHandlers{
		shareStatus: http.StatusOK,
		shareBody:   management.PublicShareStateResponse{State: "public", CPSynced: pubBoolPtr(true)},
		useStatus:   http.StatusOK,
		useBody:     management.PublicUseResponse{Mode: "auto", EffectiveMode: "auto", Consented: true},
		events:      nudgeEvents("no_candidate"),
	})

	var buf bytes.Buffer
	if err := runPublicStatus(url, false, &buf); err != nil {
		t.Fatalf("runPublicStatus: %v", err)
	}
	if strings.Contains(buf.String(), madeUpNudge) {
		t.Errorf("nudge printed despite consent\n---\n%s", buf.String())
	}
}

func TestRunPublicStatus_NudgeReasonNeverPrinted(t *testing.T) {
	const secretReason = "all_overloaded_reason_marker"
	url := newPublicServer(t, publicHandlers{
		shareStatus: http.StatusOK,
		shareBody:   management.PublicShareStateResponse{State: "not_public", CPSynced: pubBoolPtr(true)},
		useStatus:   http.StatusOK,
		useBody:     management.PublicUseResponse{Mode: "off", EffectiveMode: "off", Consented: false},
		events:      nudgeEvents(secretReason),
	})

	var buf bytes.Buffer
	if err := runPublicStatus(url, false, &buf); err != nil {
		t.Fatalf("runPublicStatus: %v", err)
	}
	if strings.Contains(buf.String(), secretReason) {
		t.Errorf("nudge Reason leaked into output\n---\n%s", buf.String())
	}
	// Sanity: the message itself still rendered.
	if !strings.Contains(buf.String(), madeUpNudge) {
		t.Errorf("nudge message missing\n---\n%s", buf.String())
	}
}

func TestRunPublicStatus_JSON(t *testing.T) {
	url := newPublicServer(t, publicHandlers{
		shareStatus: http.StatusOK,
		shareBody:   management.PublicShareStateResponse{State: "public", CPSynced: pubBoolPtr(true)},
		useStatus:   http.StatusOK,
		useBody:     management.PublicUseResponse{Mode: "auto", EffectiveMode: "auto", Consented: true},
	})

	var buf bytes.Buffer
	if err := runPublicStatus(url, true, &buf); err != nil {
		t.Fatalf("runPublicStatus json: %v", err)
	}
	var got struct {
		Share *management.PublicShareStateResponse `json:"share"`
		Use   *management.PublicUseResponse        `json:"use"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n---\n%s", err, buf.String())
	}
	if got.Share == nil || got.Share.State != "public" {
		t.Errorf("share object missing/wrong: %+v", got.Share)
	}
	if got.Use == nil || got.Use.EffectiveMode != "auto" {
		t.Errorf("use object missing/wrong: %+v", got.Use)
	}
}

func TestRunPublicStatus_JSONNullOnUnsupported(t *testing.T) {
	url := newPublicServer(t, publicHandlers{
		// both routes unregistered -> both 404
	})

	var buf bytes.Buffer
	if err := runPublicStatus(url, true, &buf); err != nil {
		t.Fatalf("runPublicStatus json: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n---\n%s", err, buf.String())
	}
	if string(got["share"]) != "null" || string(got["use"]) != "null" {
		t.Errorf("want share:null use:null, got %s", buf.String())
	}
}

func TestRunPublic_BogusSubcommand(t *testing.T) {
	if err := runPublic([]string{"bogus"}); err == nil {
		t.Fatal("runPublic([bogus]) = nil, want an error")
	}
}

// TestRunPublicStatus_GuestLimitUnset: 0 means the operator never chose
// a cap, so the control plane's automatic default applies. Say that and
// say how to change it — the previous "not reported by this daemon"
// wording described a wiring gap on our side (waired#901 L6) and read
// as a broken daemon, which invites a pointless upgrade or re-share.
func TestRunPublicStatus_GuestLimitUnset(t *testing.T) {
	url := newPublicServer(t, publicHandlers{
		shareStatus: http.StatusOK,
		shareBody: management.PublicShareStateResponse{
			State:        "public",
			DesiredState: "public",
			CPSynced:     pubBoolPtr(true),
		},
		useStatus: http.StatusOK,
		useBody:   management.PublicUseResponse{Mode: "off", EffectiveMode: "off"},
	})

	var buf bytes.Buffer
	if err := runPublicStatus(url, false, &buf); err != nil {
		t.Fatalf("runPublicStatus: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Guest limit: automatic") {
		t.Errorf("unset guest limit should read as automatic\n---\n%s", out)
	}
	if !strings.Contains(out, "--max-clients") {
		t.Errorf("unset guest limit should point at the flag that sets it\n---\n%s", out)
	}
	if strings.Contains(out, "not reported by this daemon") {
		t.Errorf("stale daemon-fault wording still present\n---\n%s", out)
	}
}

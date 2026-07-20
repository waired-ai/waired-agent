package tray

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/observabilityclient"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/platform/notification"
)

// stubNotifier records the toasts the tray emits so a test can assert the
// count and the verbatim body. It satisfies notification.Notifier.
type stubNotifier struct {
	mu    sync.Mutex
	calls []notifyCall
}

type notifyCall struct {
	title string
	body  string
	level notification.Level
}

func (s *stubNotifier) Notify(title, body string, level notification.Level) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, notifyCall{title: title, body: body, level: level})
	return nil
}

func (s *stubNotifier) snapshot() []notifyCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]notifyCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// installStubNotifier swaps the package-level notifier for a recording
// stub and restores it on cleanup. notifier is already a package var, so
// no production change is needed.
func installStubNotifier(t *testing.T) *stubNotifier {
	t.Helper()
	s := &stubNotifier{}
	orig := notifier
	notifier = s
	t.Cleanup(func() { notifier = orig })
	return s
}

// TestPollObservability_RequestsNudgeKind asserts the single shared
// /events poll widens its kinds filter to carry the pre-consent Public
// Share nudge alongside fallbacks — no second, cursor-desyncing GET.
func TestPollObservability_RequestsNudgeKind(t *testing.T) {
	installStubNotifier(t)
	var kindsSeen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/waired/v1/observability/state":
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{})
		case "/waired/v1/observability/events":
			kindsSeen = r.URL.Query().Get("kinds")
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	tr := &tray{cli: newTestClient(srv.URL), obsSupported: true}
	tr.pollObservability(context.Background(), &Snapshot{})

	for _, want := range []string{"fallback", "public_share_nudge"} {
		if !strings.Contains(kindsSeen, want) {
			t.Errorf("kinds query %q missing %q — both must ride the one /events poll", kindsSeen, want)
		}
	}
}

// TestNudgeDoesNotEnterFallbackBuffer asserts a nudge event in the batch
// is routed to the toast path, never into the recent-fallbacks buffer the
// "Recent activity" submenu renders.
func TestNudgeDoesNotEnterFallbackBuffer(t *testing.T) {
	installStubNotifier(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/waired/v1/observability/state":
			_ = json.NewEncoder(w).Encode(management.ObservabilityState{})
		case "/waired/v1/observability/events":
			_ = json.NewEncoder(w).Encode(observabilityclient.EventsResponse{
				Events: []observability.Event{
					{
						Seq:  1,
						TS:   time.Now(),
						Kind: observability.KindPublicShareNudge,
						PublicShareNudge: &observability.PublicShareNudgeEvent{
							Model:   "qwen3:8b",
							Reason:  "no_candidate",
							Message: observability.PublicShareNudgeMessage,
						},
					},
				},
				NextSince: 1,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	tr := &tray{cli: newTestClient(srv.URL), obsSupported: true}
	snap := Snapshot{}
	tr.pollObservability(context.Background(), &snap)

	if len(snap.RecentFallbacks) != 0 {
		t.Errorf("nudge leaked into RecentFallbacks: %+v", snap.RecentFallbacks)
	}
}

// TestMaybeShowPublicNudge_FiresOnce asserts the same Seq shown twice
// (successive polls / the first-poll since=0 replay) toasts only once.
func TestMaybeShowPublicNudge_FiresOnce(t *testing.T) {
	s := installStubNotifier(t)
	tr := &tray{}
	tr.maybeShowPublicNudge(observability.PublicShareNudgeMessage, 5)
	tr.maybeShowPublicNudge(observability.PublicShareNudgeMessage, 5)

	if got := len(s.snapshot()); got != 1 {
		t.Errorf("notifications=%d, want 1 — the same nudge Seq must toast only once", got)
	}
}

// TestMaybeShowPublicNudge_SuppressedWhenConsented asserts the hint is
// suppressed once consent exists — it only makes sense pre-consent.
func TestMaybeShowPublicNudge_SuppressedWhenConsented(t *testing.T) {
	s := installStubNotifier(t)
	tr := &tray{}
	tr.last.PublicUseConsented = true
	tr.maybeShowPublicNudge(observability.PublicShareNudgeMessage, 1)

	if got := len(s.snapshot()); got != 0 {
		t.Errorf("notifications=%d, want 0 — nudge must be suppressed when consent already exists", got)
	}
}

// TestMaybeShowPublicNudge_UsesMessageVerbatim asserts the toast body is
// the daemon-authored message VERBATIM and never leaks the Reason filter
// tag ("no_candidate" / "all_overloaded").
func TestMaybeShowPublicNudge_UsesMessageVerbatim(t *testing.T) {
	s := installStubNotifier(t)
	tr := &tray{}
	tr.maybeShowPublicNudge(observability.PublicShareNudgeMessage, 1)

	calls := s.snapshot()
	if len(calls) != 1 {
		t.Fatalf("notifications=%d, want 1", len(calls))
	}
	if calls[0].body != observability.PublicShareNudgeMessage {
		t.Errorf("toast body=%q, want the daemon message verbatim %q", calls[0].body, observability.PublicShareNudgeMessage)
	}
	for _, tag := range []string{"no_candidate", "all_overloaded"} {
		if strings.Contains(calls[0].body, tag) {
			t.Errorf("toast body leaked the Reason filter tag %q: %q", tag, calls[0].body)
		}
	}
}

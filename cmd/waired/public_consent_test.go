package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
)

// consentDaemon is a configurable fake Local Management API for the
// consent-flow tests. Behaviour is set by the config fields; the observed
// counters record what the CLI actually did.
type consentDaemon struct {
	// config
	warningVersion int
	warningTitle   string
	warningText    string
	acceptLabel    string
	cancelLabel    string
	warning404     bool
	useConsented   bool
	use404         bool
	// consentMismatches leading POST /consent calls return 409
	// warning_version_mismatch before the next one succeeds.
	consentMismatches int

	// observed
	mu            sync.Mutex
	warningGets   int
	consentPosts  int
	consentBodies []int
}

func (d *consentDaemon) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/waired/v1/public/use", func(w http.ResponseWriter, _ *http.Request) {
		if d.use404 {
			http.Error(w, "not configured", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(management.PublicUseResponse{
			Mode:          "off",
			EffectiveMode: "off",
			Consented:     d.useConsented,
		})
	})

	mux.HandleFunc("/waired/v1/public/warning", func(w http.ResponseWriter, _ *http.Request) {
		if d.warning404 {
			http.Error(w, "not configured", http.StatusNotFound)
			return
		}
		d.mu.Lock()
		d.warningGets++
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(management.PublicWarningResponse{
			Version:     d.warningVersion,
			Title:       d.warningTitle,
			Text:        d.warningText,
			AcceptLabel: d.acceptLabel,
			CancelLabel: d.cancelLabel,
		})
	})

	mux.HandleFunc("/waired/v1/public/consent", func(w http.ResponseWriter, r *http.Request) {
		var req management.PublicConsentRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		d.consentPosts++
		d.consentBodies = append(d.consentBodies, req.WarningVersion)
		n := d.consentPosts
		d.mu.Unlock()
		if n <= d.consentMismatches {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error_code": "warning_version_mismatch",
				"message":    "re-fetch and re-display",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(management.PublicUseResponse{
			Mode:          "auto",
			EffectiveMode: "auto",
			Main:          true,
			Sub:           true,
			Consented:     true,
		})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// forceInteractive pins the mockable stdin-is-a-terminal seam for a test,
// restoring the real detector afterwards.
func forceInteractive(t *testing.T, v bool) {
	t.Helper()
	prev := stdinIsInteractiveFn
	stdinIsInteractiveFn = func() bool { return v }
	t.Cleanup(func() { stdinIsInteractiveFn = prev })
}

// baseConsentDaemon is a sane default: not yet consented, provider
// sharing off (so reciprocity fires), warning version 1.
func baseConsentDaemon() *consentDaemon {
	return &consentDaemon{
		warningVersion: 1,
		warningTitle:   "ZZ default title",
		warningText:    "ZZ default body",
		acceptLabel:    "OK",
		cancelLabel:    "Cancel",
	}
}

func TestEnsurePublicConsent_PrintsServedWarningTextVerbatim(t *testing.T) {
	forceInteractive(t, true)
	d := baseConsentDaemon()
	// Deliberately unlike any constant in cmd/waired or internal/management:
	// the served string is the ONLY place this text exists, so seeing it in
	// the output proves the CLI printed server data, not a hardcoded copy.
	d.warningTitle = "ZZ-served-title-not-a-constant"
	d.warningText = "ZZ-served-body-line\nMore: example.invalid/zz"
	url := d.start(t)

	var out bytes.Buffer
	if _, err := ensurePublicConsent(url, &out, strings.NewReader("y\n")); err != nil {
		t.Fatalf("ensurePublicConsent: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, d.warningTitle) {
		t.Errorf("served title not printed\nwant substring: %q\n---\n%s", d.warningTitle, got)
	}
	if !strings.Contains(got, d.warningText) {
		t.Errorf("served body not printed verbatim (incl. More: line)\nwant substring: %q\n---\n%s", d.warningText, got)
	}
}

func TestEnsurePublicConsent_PostsConsentWithServedVersion(t *testing.T) {
	forceInteractive(t, true)
	d := baseConsentDaemon()
	d.warningVersion = 7
	url := d.start(t)

	var out bytes.Buffer
	if _, err := ensurePublicConsent(url, &out, strings.NewReader("y\n")); err != nil {
		t.Fatalf("ensurePublicConsent: %v", err)
	}
	if len(d.consentBodies) != 1 || d.consentBodies[0] != 7 {
		t.Errorf("consent bodies = %v, want exactly [7]", d.consentBodies)
	}
}

func TestEnsurePublicConsent_DeclineRecordsNothing(t *testing.T) {
	forceInteractive(t, true)
	d := baseConsentDaemon()
	url := d.start(t)

	var out bytes.Buffer
	_, err := ensurePublicConsent(url, &out, strings.NewReader("n\n"))
	if !errors.Is(err, errPublicConsentDeclined) {
		t.Fatalf("err = %v, want errPublicConsentDeclined", err)
	}
	if d.consentPosts != 0 {
		t.Errorf("consent POSTs = %d, want 0 on decline", d.consentPosts)
	}
}

func TestEnsurePublicConsent_VersionMismatchRefetchesOnce(t *testing.T) {
	forceInteractive(t, true)
	d := baseConsentDaemon()
	d.consentMismatches = 1 // first POST 409s, second succeeds
	url := d.start(t)

	var out bytes.Buffer
	if _, err := ensurePublicConsent(url, &out, strings.NewReader("y\ny\n")); err != nil {
		t.Fatalf("ensurePublicConsent: %v", err)
	}
	if d.warningGets != 2 {
		t.Errorf("warning GETs = %d, want 2 (a re-fetch after the mismatch)", d.warningGets)
	}
	if d.consentPosts != 2 {
		t.Errorf("consent POSTs = %d, want 2", d.consentPosts)
	}
}

func TestEnsurePublicConsent_SecondMismatchIsHardError(t *testing.T) {
	forceInteractive(t, true)
	d := baseConsentDaemon()
	d.consentMismatches = 2 // both attempts 409
	url := d.start(t)

	var out bytes.Buffer
	_, err := ensurePublicConsent(url, &out, strings.NewReader("y\ny\n"))
	if err == nil {
		t.Fatal("ensurePublicConsent = nil, want a hard error on the second mismatch")
	}
	if errors.Is(err, errPublicConsentDeclined) {
		t.Fatalf("err = %v, want a hard error, not a decline", err)
	}
	if d.consentPosts != 2 {
		t.Errorf("consent POSTs = %d, want exactly 2 (no third attempt)", d.consentPosts)
	}
}

func TestEnsurePublicConsent_NonInteractiveRefusesEvenWithYes(t *testing.T) {
	forceInteractive(t, false) // simulate a pipe / non-TTY stdin
	d := baseConsentDaemon()
	url := d.start(t)

	var out bytes.Buffer
	// "y" on stdin stands in for a --yes-style attempt to auto-accept; the
	// guard must still refuse and record nothing.
	_, err := ensurePublicConsent(url, &out, strings.NewReader("y\n"))
	if err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("err = %v, want a refusal mentioning a terminal", err)
	}
	if d.consentPosts != 0 {
		t.Errorf("consent POSTs = %d, want 0 when non-interactive", d.consentPosts)
	}
}

func TestEnsurePublicConsent_SkipsPromptWhenAlreadyConsented(t *testing.T) {
	// Interactive detector intentionally left at its real value: an
	// already-consented computer must not prompt at all.
	d := baseConsentDaemon()
	d.useConsented = true
	url := d.start(t)

	var out bytes.Buffer
	resp, err := ensurePublicConsent(url, &out, strings.NewReader(""))
	if err != nil {
		t.Fatalf("ensurePublicConsent: %v", err)
	}
	if !resp.Consented {
		t.Errorf("resp.Consented = false, want true")
	}
	if d.warningGets != 0 || d.consentPosts != 0 {
		t.Errorf("already-consented path touched the flow: warningGets=%d consentPosts=%d",
			d.warningGets, d.consentPosts)
	}
}

// waired#1297: using other people's computers still requires sharing one
// of yours, but this command can no longer turn that on — the setting is
// the console's. Accepting must say where it is instead of silently
// leaving the person ineligible.
func TestEnsurePublicConsent_PointsAtTheConsoleForReciprocity(t *testing.T) {
	forceInteractive(t, true)
	d := baseConsentDaemon()
	url := d.start(t)

	var out bytes.Buffer
	if _, err := ensurePublicConsent(url, &out, strings.NewReader("y\n")); err != nil {
		t.Fatalf("ensurePublicConsent: %v", err)
	}
	if !strings.Contains(out.String(), publicReciprocityNote) {
		t.Errorf("the reciprocity note was not printed\nwant: %q\n---\n%s", publicReciprocityNote, out.String())
	}
}

func TestEnsurePublicConsent_WarningUnsupported404ReturnsSentinel(t *testing.T) {
	forceInteractive(t, true)
	d := baseConsentDaemon()
	d.warning404 = true
	url := d.start(t)

	var out bytes.Buffer
	_, err := ensurePublicConsent(url, &out, strings.NewReader("y\n"))
	if !errors.Is(err, errPublicUseUnsupported) {
		t.Fatalf("err = %v, want errPublicUseUnsupported", err)
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// useDaemon is a fake Local Management API for the `public use` tests. It
// captures the POST /use body and can be told to demand consent once
// (409) before accepting a write.
type useDaemon struct {
	consented       bool
	requireConsent1 bool // first POST /use 409s consent_required, then accepts

	mu           sync.Mutex
	useGets      int
	usePosts     int
	usePostBody  []byte
	consentPosts int
}

func (d *useDaemon) start(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/waired/v1/public/use", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			d.mu.Lock()
			d.useGets++
			consented := d.consented
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(management.PublicUseResponse{
				Mode:          "auto",
				EffectiveMode: "auto",
				Consented:     consented,
			})
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			d.mu.Lock()
			d.usePosts++
			n := d.usePosts
			d.usePostBody = body
			d.mu.Unlock()
			if d.requireConsent1 && n == 1 {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error_code": "consent_required",
					"message":    "accept the warning first",
				})
				return
			}
			_ = json.NewEncoder(w).Encode(management.PublicUseResponse{
				Mode:          "auto",
				EffectiveMode: "auto",
				Consented:     true,
			})
		}
	})

	// Consent-flow routes (only exercised by the 409 retry test).
	mux.HandleFunc("/waired/v1/public/warning", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(management.PublicWarningResponse{
			Version: 1, Title: "ZZ title", Text: "ZZ text", AcceptLabel: "OK", CancelLabel: "Cancel",
		})
	})
	mux.HandleFunc("/waired/v1/public/consent", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		d.consentPosts++
		d.consented = true
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(management.PublicUseResponse{Consented: true})
	})
	// The consent flow used to turn public sharing on from here, and this
	// stood in for the daemon refusing. Since waired#1297 it only prints
	// where the setting lives, so there is nothing left for the route to
	// answer — it stays as a 404 so a request to it would still be caught
	// rather than silently succeeding against the mux's fallthrough.
	mux.HandleFunc("/waired/v1/public/share", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not configured", http.StatusNotFound)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestRunPublicUse_ShowsSettingsWithNoFlags(t *testing.T) {
	d := &useDaemon{consented: true}
	url := d.start(t)

	var out bytes.Buffer
	if err := runPublicUse(url, management.PublicUseUpdateRequest{}, false, false, &out, strings.NewReader("")); err != nil {
		t.Fatalf("runPublicUse: %v", err)
	}
	if d.usePosts != 0 {
		t.Errorf("POSTs = %d, want 0 (no flags = viewer only)", d.usePosts)
	}
	if !strings.Contains(out.String(), "Use public nodes: auto") {
		t.Errorf("settings not rendered\n---\n%s", out.String())
	}
}

func TestNewPublicUseCmd_ModeFlagsMutuallyExclusive(t *testing.T) {
	cmd := newPublicUseCmd()
	cmd.SetArgs([]string{"--auto", "--off"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "choose only one") {
		t.Fatalf("err = %v, want a mutual-exclusion error", err)
	}
}

func TestNewPublicUseCmd_SendsOnlyChangedFields(t *testing.T) {
	d := &useDaemon{consented: true}
	url := d.start(t)

	cmd := newPublicUseCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--mgmt", url, "--min-model-size", "medium"})
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := strings.TrimSpace(string(d.usePostBody)); got != `{"min_model_size":"medium"}` {
		t.Errorf("POST body = %s, want exactly {\"min_model_size\":\"medium\"}", got)
	}
}

// TestNewPublicUseCmd_RejectsUnknownSize: the floor is enforced by rank
// and an unrecognised word ranks as unknown, which would quietly mean "no
// floor" — so it is rejected at the flag rather than sent.
func TestNewPublicUseCmd_RejectsUnknownSize(t *testing.T) {
	cmd := newPublicUseCmd()
	cmd.SetArgs([]string{"--min-model-size", "enormous"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "small, medium or large") {
		t.Fatalf("err = %v, want a vocabulary rejection", err)
	}
}

func TestNewPublicUseCmd_MainSubAcceptOnOff(t *testing.T) {
	d := &useDaemon{consented: true}
	url := d.start(t)

	cmd := newPublicUseCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetIn(strings.NewReader(""))
	cmd.SetArgs([]string{"--mgmt", url, "--main", "off", "--sub", "on"})
	cmd.SilenceUsage = true
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := strings.TrimSpace(string(d.usePostBody)); got != `{"main":false,"sub":true}` {
		t.Errorf("POST body = %s, want exactly {\"main\":false,\"sub\":true}", got)
	}
}

func TestNewPublicUseCmd_RejectsInvalidOnOff(t *testing.T) {
	cmd := newPublicUseCmd()
	cmd.SetArgs([]string{"--main", "bogus"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "on or off") {
		t.Fatalf("err = %v, want an on|off rejection", err)
	}
}

// TestNewPublicUseCmd_MinTierIsRetired: --min-tier still parses so a
// script that passes it is told what changed instead of failing on an
// unknown flag, and it never silently writes a floor the daemon no longer
// reads (#537, #200's treatment of a retired name).
func TestNewPublicUseCmd_MinTierIsRetired(t *testing.T) {
	cmd := newPublicUseCmd()
	cmd.SetArgs([]string{"--min-tier", "40"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--min-model-size") {
		t.Fatalf("err = %v, want a rejection naming the replacement flag", err)
	}
}

func TestRunPublicUse_ConsentRequired409TriggersConsentFlowAndRetries(t *testing.T) {
	forceInteractive(t, true)
	// Not yet consented: the first ensurePublicConsent records consent, but
	// the daemon still 409s the first write (consent_required — a raced
	// warning bump), so runPublicUse must run the flow again and retry the
	// write once.
	d := &useDaemon{consented: false, requireConsent1: true}
	url := d.start(t)

	var out bytes.Buffer
	upd := management.PublicUseUpdateRequest{MinModelSize: strPtr(hostfit.ModelSizeMedium)}
	if err := runPublicUse(url, upd, true, false, &out, strings.NewReader("y\n")); err != nil {
		t.Fatalf("runPublicUse: %v", err)
	}
	if d.consentPosts != 1 {
		t.Errorf("consent POSTs = %d, want 1 (consent recorded before the write)", d.consentPosts)
	}
	if d.usePosts != 2 {
		t.Errorf("use POSTs = %d, want 2 (original 409 + retry)", d.usePosts)
	}
}

func strPtr(s string) *string { return &s }

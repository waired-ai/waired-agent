package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/identity"
)

// Labels carry a leading emoji / ASCII marker (see initStepLabels); the
// assertions check the "[i/N]" token is present rather than exact-matching
// so the cosmetic prefix can change freely.
func TestInitStepLabelsRenew(t *testing.T) {
	got := initStepLabels(true)
	if !strings.Contains(got.signIn, "[1/3]") {
		t.Errorf("renew signIn want to contain [1/3] got %q", got.signIn)
	}
	if !strings.Contains(got.register, "[2/3]") {
		t.Errorf("renew register want to contain [2/3] got %q", got.register)
	}
	if !strings.Contains(got.persist, "[3/3]") {
		t.Errorf("renew persist want to contain [3/3] got %q", got.persist)
	}
	if got.inference != "" {
		t.Errorf("renew should skip inference prompt; got label %q", got.inference)
	}
	if got.integration != "" {
		t.Errorf("renew should skip integration prompt; got label %q", got.integration)
	}
}

func TestInitStepLabelsFresh(t *testing.T) {
	got := initStepLabels(false)
	if !strings.Contains(got.signIn, "[1/4]") {
		t.Errorf("fresh signIn want to contain [1/4] got %q", got.signIn)
	}
	if !strings.Contains(got.inference, "[3a/4]") {
		t.Errorf("fresh inference want to contain [3a/4] got %q", got.inference)
	}
	if !strings.Contains(got.integration, "[3b/4]") {
		t.Errorf("fresh integration want to contain [3b/4] got %q", got.integration)
	}
	if !strings.Contains(got.done, "[4/4]") {
		t.Errorf("fresh done want to contain [4/4] got %q", got.done)
	}
}

func TestConfirmRenewBypassSkipsPrompt(t *testing.T) {
	id := &identity.Identity{
		AccountEmail: "alice@example.com",
		DeviceName:   "alice-mac",
		NetworkName:  "alice",
		ControlURL:   "https://cp.example.com",
	}
	var out bytes.Buffer
	got := confirmRenew(strings.NewReader(""), &out, id, true, false)
	if !got {
		t.Fatalf("bypass mode should auto-confirm renew, got false")
	}
	if !strings.Contains(out.String(), "alice@example.com") {
		t.Errorf("summary should include the account email; out=%q", out.String())
	}
	if !strings.Contains(out.String(), "non-interactive") {
		t.Errorf("bypass should announce non-interactive proceed; out=%q", out.String())
	}
}

func TestConfirmRenewNonInteractiveSkipsPrompt(t *testing.T) {
	id := &identity.Identity{AccountEmail: "alice@example.com"}
	var out bytes.Buffer
	got := confirmRenew(strings.NewReader(""), &out, id, false, true)
	if !got {
		t.Fatalf("--non-interactive should auto-confirm renew")
	}
}

func TestConfirmRenewInteractiveDefaultsToYes(t *testing.T) {
	id := &identity.Identity{AccountEmail: "alice@example.com"}
	var out bytes.Buffer
	// Empty answer → ynPrompt returns the default (true).
	got := confirmRenew(strings.NewReader("\n"), &out, id, false, false)
	if !got {
		t.Fatalf("empty input should fall through to default Y")
	}
}

func TestConfirmRenewInteractiveRejected(t *testing.T) {
	id := &identity.Identity{AccountEmail: "alice@example.com"}
	var out bytes.Buffer
	got := confirmRenew(strings.NewReader("n\n"), &out, id, false, false)
	if got {
		t.Fatalf("n response must abort renew")
	}
}

func TestConfirmRenewInteractiveAccepted(t *testing.T) {
	id := &identity.Identity{AccountEmail: "alice@example.com"}
	var out bytes.Buffer
	got := confirmRenew(strings.NewReader("y\n"), &out, id, false, false)
	if !got {
		t.Fatalf("y response must continue with renew")
	}
}

func TestConfirmRenewSummaryFallbacksToDash(t *testing.T) {
	id := &identity.Identity{
		// All optional fields empty — exercise the "-" fallback path.
		DeviceID: "dev_abc",
	}
	var out bytes.Buffer
	confirmRenew(strings.NewReader(""), &out, id, true, false)
	// DeviceID fallback for DeviceName.
	if !strings.Contains(out.String(), "Device:  dev_abc") {
		t.Errorf("DeviceID should be used when DeviceName is empty; out=%q", out.String())
	}
	if !strings.Contains(out.String(), "Account: -") {
		t.Errorf("missing email should render as '-'; out=%q", out.String())
	}
}

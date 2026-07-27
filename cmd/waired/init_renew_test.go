package main

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/identity"
)

func TestConfirmRenewBypassSkipsPrompt(t *testing.T) {
	id := &identity.Identity{
		AccountEmail: "alice@example.com",
		DeviceName:   "alice-mac",
		NetworkName:  "alice",
		ControlURL:   "https://cp.example.com",
	}
	var out bytes.Buffer
	got := confirmRenew(bufio.NewScanner(strings.NewReader("")), &out, id, true, false)
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
	got := confirmRenew(bufio.NewScanner(strings.NewReader("")), &out, id, false, true)
	if !got {
		t.Fatalf("--non-interactive should auto-confirm renew")
	}
}

func TestConfirmRenewInteractiveDefaultsToYes(t *testing.T) {
	id := &identity.Identity{AccountEmail: "alice@example.com"}
	var out bytes.Buffer
	// Empty answer → ynPrompt returns the default (true).
	got := confirmRenew(bufio.NewScanner(strings.NewReader("\n")), &out, id, false, false)
	if !got {
		t.Fatalf("empty input should fall through to default Y")
	}
}

func TestConfirmRenewInteractiveRejected(t *testing.T) {
	id := &identity.Identity{AccountEmail: "alice@example.com"}
	var out bytes.Buffer
	got := confirmRenew(bufio.NewScanner(strings.NewReader("n\n")), &out, id, false, false)
	if got {
		t.Fatalf("n response must abort renew")
	}
}

func TestConfirmRenewInteractiveAccepted(t *testing.T) {
	id := &identity.Identity{AccountEmail: "alice@example.com"}
	var out bytes.Buffer
	got := confirmRenew(bufio.NewScanner(strings.NewReader("y\n")), &out, id, false, false)
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
	confirmRenew(bufio.NewScanner(strings.NewReader("")), &out, id, true, false)
	// DeviceID fallback for DeviceName.
	if !strings.Contains(out.String(), "Device:  dev_abc") {
		t.Errorf("DeviceID should be used when DeviceName is empty; out=%q", out.String())
	}
	if !strings.Contains(out.String(), "Account: -") {
		t.Errorf("missing email should render as '-'; out=%q", out.String())
	}
}

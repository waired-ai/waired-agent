package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/platform/appcontrol"
)

func TestAppControlFindingSaysNothingWhenThereIsNothingToSay(t *testing.T) {
	for _, r := range []appcontrol.Result{
		{},                                 // zero value: NotApplicable
		{Status: appcontrol.NotApplicable}, // off Windows, or the log could not be read
		{Status: appcontrol.Clear},         // read, and nothing was refused
		{Status: appcontrol.Refused},       // Refused with no entries is not a claim
	} {
		if f := appControlFinding(r); f.Subject != "" {
			t.Errorf("status %v produced a finding: %+v", r.Status, f)
		}
	}
}

func TestAppControlFindingIsAWarnThatNamesTheFile(t *testing.T) {
	r := appcontrol.Result{
		Status: appcontrol.Refused,
		Refusals: []appcontrol.Refusal{{
			Program:           "waired.exe",
			Count:             234,
			Requesters:        []string{"bash.exe"},
			AnsweredFromCache: true,
		}},
	}
	f := appControlFinding(r)
	if f.Subject != "Windows Application Control" {
		t.Errorf("Subject = %q", f.Subject)
	}
	// Warn, not Fail. There is nothing to repair, and doctor exiting non-zero
	// would tell the user their machine is broken when the answer is to wait.
	if f.Status != integration.StatusWarn {
		t.Errorf("Status = %v, want Warn", f.Status)
	}
	for _, want := range []string{
		"waired.exe",
		"234 refused launches",
		"per file",
		"bash.exe",                          // evidence: who was turned away
		"answered from this device's cache", // evidence: 3118 said no cloud call
	} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail does not mention %q:\n%s", want, f.Detail)
		}
	}
}

// TestAppControlEvidenceDoesNotClaimAnUnreadReputationAnswer. With no event
// 3118 in the log, neither "the cloud was asked" nor "the cache answered" is
// known, and the evidence clause must say neither.
func TestAppControlEvidenceDoesNotClaimAnUnreadReputationAnswer(t *testing.T) {
	r := appcontrol.Result{
		Status:   appcontrol.Refused,
		Refusals: []appcontrol.Refusal{{Program: "waired-agent.exe", Count: 2, Requesters: []string{"services.exe"}}},
	}
	ev := appControlEvidence(r)
	if strings.Contains(ev, "cache") || strings.Contains(ev, "lookup") {
		t.Errorf("evidence claims a reputation answer that was never read:\n%s", ev)
	}
	if !strings.Contains(ev, "services.exe") {
		t.Errorf("evidence should still name the requester:\n%s", ev)
	}
}

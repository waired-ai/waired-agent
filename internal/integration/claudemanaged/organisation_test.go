package claudemanaged

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func seedManaged(t *testing.T, body string) string {
	t.Helper()
	p := withTempPath(t)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestOrgManagedAtRecognisesTheSignals: each key means the same thing —
// somebody who is not the person at this keyboard configured Claude Code on
// this machine (waired-agent#1188).
func TestOrgManagedAtRecognisesTheSignals(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string // the key expected in the signals, "" for none
	}{
		{"absent", "", ""},
		{"empty object", `{}`, ""},
		{"waired's own write", `{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:9472"}}`, ""},
		{"forced org", `{"forceLoginOrgUUID":"11111111-2222-3333-4444-555555555555"}`, "forceLoginOrgUUID"},
		{"forced login method", `{"forceLoginMethod":"console"}`, "forceLoginMethod"},
		{"forced gateway", `{"forceLoginGatewayUrl":"https://gw.corp.example"}`, "forceLoginGatewayUrl"},
		{"model allowlist", `{"availableModels":["claude-opus-4-8"]}`, "availableModels"},
		{"picker lineup", `{"modelPicker":{"options":[{"model":"opus"}]}}`, "modelPicker"},
		{"somebody else's gateway", `{"env":{"ANTHROPIC_BASE_URL":"https://gw.corp.example/v1"}}`, "ANTHROPIC_BASE_URL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := withTempPath(t)
			if tc.body != "" {
				p = seedManaged(t, tc.body)
			}
			got := OrgManagedAt(p)
			if tc.want == "" {
				if len(got) != 0 {
					t.Errorf("OrgManagedAt = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || got[0].Key != tc.want {
				t.Errorf("OrgManagedAt = %v, want one signal for %q", got, tc.want)
			}
		})
	}
}

// TestWriteRefusesAnOrganisationManagedFile is the point of the lane. The
// write is not merely inconvenient on such a machine: a non-default
// ANTHROPIC_BASE_URL is documented as a way server-managed settings are
// BYPASSED, so it would switch off the policy the organisation delivers to
// every session on the host.
func TestWriteRefusesAnOrganisationManagedFile(t *testing.T) {
	body := `{"forceLoginOrgUUID":"11111111-2222-3333-4444-555555555555",` +
		`"env":{"ANTHROPIC_BASE_URL":"https://gw.corp.example/v1"}}`
	p := seedManaged(t, body)

	_, err := Write("http://127.0.0.1:9472")
	var org *ErrOrgManaged
	if !errors.As(err, &org) {
		t.Fatalf("Write = %v, want an ErrOrgManaged refusal", err)
	}
	if len(org.Signals) != 2 {
		t.Errorf("signals = %v, want both the forced org and the foreign base URL", org.Signals)
	}
	// The message has to name what was found, or the operator cannot tell
	// which of their own settings triggered it.
	if !strings.Contains(org.Error(), "forceLoginOrgUUID") {
		t.Errorf("message = %q, want it to name the key", org.Error())
	}

	// And nothing was written.
	got, _ := os.ReadFile(p)
	if string(got) != body {
		t.Errorf("the file changed anyway:\n%s", got)
	}
}

// The asymmetry this closes: Remove has always refused to delete somebody
// else's base URL, while Write overwrote it. Both directions now agree.
func TestWriteAndRemoveAgreeAboutWhoOwnsTheBaseURL(t *testing.T) {
	body := `{"env":{"ANTHROPIC_BASE_URL":"https://gw.corp.example/v1"}}`
	p := seedManaged(t, body)

	if _, err := Write("http://127.0.0.1:9472"); err == nil {
		t.Error("Write overwrote a base URL waired did not put there")
	}
	if _, err := Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "gw.corp.example") {
		t.Errorf("Remove deleted a base URL waired did not put there: %s", got)
	}
}

// A file waired already owns is not "organisation managed" just because it
// exists — re-running enable on an ordinary host must keep working.
func TestWriteStillWorksOnAHostItAlreadyConfigured(t *testing.T) {
	withTempPath(t)
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if _, err := Write("http://127.0.0.1:9472"); err != nil {
		t.Fatalf("second Write: %v", err)
	}
}

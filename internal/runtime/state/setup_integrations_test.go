package state_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func TestReadSetupIntegrationsMissingIsEmpty(t *testing.T) {
	got, err := state.ReadSetupIntegrations(t.TempDir())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Targets) != 0 || got.WrittenAt != "" {
		t.Fatalf("missing file should read as the zero record, got %+v", got)
	}
}

func TestWriteSetupIntegrationsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := state.SetupIntegrations{
		Targets:   []string{"openclaw", "claude-code"},
		WrittenAt: "2026-08-02T10:00:00Z",
	}
	if err := state.WriteSetupIntegrations(dir, want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := state.ReadSetupIntegrations(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Normalised on the way in: sorted and de-duplicated, so a reordered
	// instruction is not a different record.
	if len(got.Targets) != 2 || got.Targets[0] != "claude-code" || got.Targets[1] != "openclaw" {
		t.Fatalf("targets = %v, want [claude-code openclaw]", got.Targets)
	}
	if got.WrittenAt != want.WrittenAt {
		t.Fatalf("written_at = %q, want %q", got.WrittenAt, want.WrittenAt)
	}
	if _, err := os.Stat(state.SetupIntegrationsPath(dir)); err != nil {
		t.Fatalf("stat %s: %v", state.SetupIntegrationsPath(dir), err)
	}
}

func TestWriteSetupIntegrationsNormalises(t *testing.T) {
	dir := t.TempDir()
	if err := state.WriteSetupIntegrations(dir, state.SetupIntegrations{
		Targets: []string{"openclaw", "", "claude-code", "openclaw"},
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := state.ReadSetupIntegrations(dir)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got.Targets) != 2 {
		t.Fatalf("targets = %v, want two entries", got.Targets)
	}
}

func TestWriteSetupIntegrationsRejectsEmpty(t *testing.T) {
	// Product contract: "nothing was written" is the ABSENCE of the file,
	// never a file with an empty list. A record that covers nothing would
	// read back as a record, and Covers has to be able to say no.
	dir := t.TempDir()
	if err := state.WriteSetupIntegrations(dir, state.SetupIntegrations{}); err == nil {
		t.Fatal("write with no targets = nil, want an error")
	}
	if _, err := os.Stat(state.SetupIntegrationsPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("rejected write must leave no file, stat err = %v", err)
	}
}

func TestReadSetupIntegrationsMalformedErrors(t *testing.T) {
	dir := t.TempDir()
	path := state.SetupIntegrationsPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("{nope"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := state.ReadSetupIntegrations(dir); err == nil {
		t.Fatal("read of malformed JSON = nil error, want one")
	}
}

func TestSetupIntegrationsCovers(t *testing.T) {
	// Product contract. Covers is what lets a restarted daemon report the
	// coding-tools row DONE without an executor attached, so each of these
	// answers is a statement about what the wizard is shown.
	for _, tc := range []struct {
		name   string
		record []string
		want   []string
		covers bool
	}{
		{"exact match", []string{"claude-code", "openclaw"}, []string{"claude-code", "openclaw"}, true},
		{"record is a superset", []string{"claude-code", "openclaw"}, []string{"claude-code"}, true},
		{"record is a subset", []string{"claude-code"}, []string{"claude-code", "openclaw"}, false},
		{"disjoint", []string{"claude-code"}, []string{"openclaw"}, false},
		{"order does not matter", []string{"openclaw", "claude-code"}, []string{"claude-code"}, true},
		// No record at all never covers anything, including an empty want:
		// the caller uses Covers to ask "did an executor write this row",
		// and a zero record means nobody has.
		{"no record", nil, []string{"claude-code"}, false},
		{"no record, nothing wanted", nil, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := state.SetupIntegrations{Targets: tc.record}
			if got := r.Covers(tc.want); got != tc.covers {
				t.Fatalf("Covers(%v) over %v = %v, want %v", tc.want, tc.record, got, tc.covers)
			}
		})
	}
}

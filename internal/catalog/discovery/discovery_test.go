package discovery

import (
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog/hfclient"
)

func TestFilter(t *testing.T) {
	const recent = "2026-06-01T00:00:00Z"
	const old = "2025-01-01T00:00:00Z"
	cutoff, _ := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")

	apache := func(id, created, pipeline string) hfclient.HubModel {
		m := hfclient.HubModel{ID: id, CreatedAt: created, PipelineTag: pipeline}
		m.CardData.License = "apache-2.0"
		return m
	}

	gated := apache("NewOrg/Gated", recent, "text-generation")
	gated.Gated = "manual"

	badLicense := apache("NewOrg/BadLicense", recent, "text-generation")
	badLicense.CardData.License = "cc-by-nc-4.0"

	tagLicense := hfclient.HubModel{ID: "NewOrg/TagLicensed", CreatedAt: recent, PipelineTag: "text-generation", Tags: []string{"license:mit"}}

	models := []hfclient.HubModel{
		apache("NewOrg/Good-Coder-32B", recent, "text-generation"), // PASS
		tagLicense, // PASS (license via tag)
		badLicense, // reject: license
		gated,      // reject: gated
		apache("NewOrg/Old-Model", old, "text-generation"),                     // reject: old
		apache("NewOrg/Vision", recent, "image-classification"),                // reject: not text-gen
		apache("Qwen/Qwen3-Coder-30B-A3B-Instruct", recent, "text-generation"), // reject: in catalog
		apache("NewOrg/Dismissed", recent, "text-generation"),                  // reject: ledger dismissed
	}

	led := Ledger{Schema: 1, Entries: map[string]SeenEntry{
		"NewOrg/Dismissed": {Status: StatusDismissed, FirstSeen: old, LastSeen: old},
	}}
	opts := FilterOpts{
		AllowedLicenses: []string{"apache-2.0", "mit"},
		CreatedAfter:    cutoff,
		Known:           map[string]bool{"qwen/qwen3-coder-30b-a3b-instruct": true},
		Ledger:          led,
	}

	got := Filter(models, opts)
	gotIDs := map[string]bool{}
	for _, c := range got {
		gotIDs[c.RepoID] = true
	}
	want := []string{"NewOrg/Good-Coder-32B", "NewOrg/TagLicensed"}
	if len(got) != len(want) {
		t.Fatalf("Filter returned %d candidates, want %d: %+v", len(got), len(want), got)
	}
	for _, w := range want {
		if !gotIDs[w] {
			t.Errorf("expected %q to pass, got %+v", w, gotIDs)
		}
	}
	// ConfigURL is populated for a passing candidate.
	for _, c := range got {
		if c.ConfigURL == "" || c.Author == "" {
			t.Errorf("candidate %s missing ConfigURL/Author: %+v", c.RepoID, c)
		}
	}
}

func TestFilter_FlaggedSkipped(t *testing.T) {
	m := hfclient.HubModel{ID: "Org/Flagged", CreatedAt: "2026-06-01T00:00:00Z", PipelineTag: "text-generation"}
	m.CardData.License = "mit"
	led := Ledger{Entries: map[string]SeenEntry{"Org/Flagged": {Status: StatusFlagged}}}
	got := Filter([]hfclient.HubModel{m}, FilterOpts{AllowedLicenses: []string{"mit"}, Ledger: led})
	if len(got) != 0 {
		t.Errorf("flagged repo should be skipped, got %+v", got)
	}
}

func TestLedger_RoundTrip(t *testing.T) {
	l, err := LoadLedger(nil)
	if err != nil {
		t.Fatalf("LoadLedger(nil): %v", err)
	}
	if l.ShouldSkip("x/y") {
		t.Error("empty ledger should not skip")
	}
	l.Record("x/y", StatusCandidate, "2026-06-18T00:00:00Z")
	if l.ShouldSkip("x/y") {
		t.Error("candidate status should not skip")
	}
	l.Record("x/y", StatusFlagged, "2026-06-19T00:00:00Z")
	if !l.ShouldSkip("x/y") {
		t.Error("flagged status should skip")
	}
	// first_seen preserved across the update.
	if l.Entries["x/y"].FirstSeen != "2026-06-18T00:00:00Z" {
		t.Errorf("first_seen not preserved: %+v", l.Entries["x/y"])
	}

	data, err := l.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	back, err := LoadLedger(data)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !back.ShouldSkip("x/y") {
		t.Error("round-trip lost flagged status")
	}
}

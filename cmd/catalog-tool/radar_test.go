package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRadarSubcommand(t *testing.T) {
	// A fresh candidate plus a repo already in the bundled catalog (must be
	// excluded by the known-catalog filter).
	const list = `[
		{"id":"NewOrg/Fresh-Coder-32B","createdAt":"2026-06-10T00:00:00Z","pipeline_tag":"text-generation","gated":false,"cardData":{"license":"apache-2.0"}},
		{"id":"Qwen/Qwen3-Coder-30B-A3B-Instruct","createdAt":"2026-06-10T00:00:00Z","pipeline_tag":"text-generation","gated":false,"cardData":{"license":"apache-2.0"}}
	]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" {
			_, _ = w.Write([]byte(list))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	ledgerPath := filepath.Join(t.TempDir(), "seen.json")
	out, err := captureStdout(t, func() error {
		return runRadar([]string{
			"--orgs", "NewOrg",
			"--hf-base-url", srv.URL,
			"--ledger", ledgerPath,
			"--since-days", "90",
			"--now", "2026-06-18T00:00:00Z",
			"--record",
		})
	})
	if err != nil {
		t.Fatalf("runRadar: %v", err)
	}
	var res radarOutput
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("parse radar output: %v\n%s", err, out)
	}
	if len(res.Candidates) != 1 || res.Candidates[0].RepoID != "NewOrg/Fresh-Coder-32B" {
		t.Fatalf("expected only NewOrg/Fresh-Coder-32B, got %+v", res.Candidates)
	}
	if res.Candidates[0].ConfigURL == "" {
		t.Error("candidate missing ConfigURL")
	}

	// --record wrote the candidate to the ledger; a second run skips nothing
	// (candidate status re-surfaces) but the ledger is now populated.
	data, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	if !json.Valid(data) {
		t.Error("ledger is not valid JSON")
	}
	if !strings.Contains(string(data), "NewOrg/Fresh-Coder-32B") {
		t.Errorf("ledger did not record the candidate: %s", data)
	}
}

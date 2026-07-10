package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// researchFixture covers each classification path: escalate, report (low
// confidence), report (too few sources), reject (malformed).
const researchFixture = `[
  {
    "repo_id": "NewOrg/Fresh-Coder-32B",
    "swe_bench_verified": 74.5,
    "secondary": {"livecodebench": 60.1},
    "sources": [
      {"url": "https://www.swebench.com/", "retrieved": "2026-06-18", "value": 74.5},
      {"url": "https://newvendor.example/blog", "retrieved": "2026-06-18", "value": 74.0}
    ],
    "confidence": "high",
    "license": "apache-2.0",
    "recommended": true,
    "rationale": "Beats the current 30B MoE on SWE-bench Verified at a 24GB-class footprint.",
    "model": {
      "model_id": "fresh-coder-32b",
      "display_name": "Fresh Coder 32B",
      "context_length": 131072,
      "variants": [
        {"variant_id": "q4-gguf", "format": "ollama-tag", "quantization": "Q4_K_M",
         "runtime_support": ["ollama"], "source": {"type": "ollama", "tag": "fresh-coder:32b-q4_K_M"},
         "total_params": 32000000000, "config_repo": "NewOrg/Fresh-Coder-32B"}
      ]
    }
  },
  {
    "repo_id": "NewOrg/Maybe-Coder-7B",
    "swe_bench_verified": 40.0,
    "sources": [{"url": "https://forum.example/post", "retrieved": "2026-06-18", "value": 40.0}],
    "confidence": "low",
    "recommended": true,
    "rationale": "Single unverified forum number.",
    "model": {"model_id": "maybe-coder-7b", "context_length": 32768, "variants": []}
  },
  {
    "repo_id": "NewOrg/Thin-Sourced-13B",
    "swe_bench_verified": 55.0,
    "sources": [{"url": "https://only-one.example", "retrieved": "2026-06-18", "value": 55.0}],
    "confidence": "high",
    "recommended": true,
    "rationale": "Only one source.",
    "model": {"model_id": "thin-13b", "context_length": 32768, "variants": []}
  },
  {
    "repo_id": "NewOrg/Bad-Confidence",
    "swe_bench_verified": 50.0,
    "sources": [],
    "confidence": "totally",
    "recommended": false,
    "rationale": "Malformed confidence."
  }
]`

func TestProposeSubcommand(t *testing.T) {
	dir := t.TempDir()
	research := writeTemp(t, "research.json", researchFixture)
	specDir := filepath.Join(dir, "specs")
	issueOut := filepath.Join(dir, "issue.md")

	out, err := captureStdout(t, func() error {
		return runPropose([]string{"--research", research, "--spec-dir", specDir, "--issue-out", issueOut})
	})
	if err != nil {
		t.Fatalf("runPropose: %v", err)
	}
	var sum proposeSummary
	if err := json.Unmarshal([]byte(out), &sum); err != nil {
		t.Fatalf("parse summary: %v\n%s", err, out)
	}

	if len(sum.Escalated) != 1 || sum.Escalated[0] != "fresh-coder-32b" {
		t.Errorf("escalated = %v, want [fresh-coder-32b]", sum.Escalated)
	}
	// Maybe-Coder (low conf) and Thin-Sourced (1 source) are reported, not escalated.
	if len(sum.Reported) != 2 {
		t.Errorf("reported = %v, want 2 entries", sum.Reported)
	}
	if len(sum.Rejected) != 1 || sum.Rejected[0].RepoID != "NewOrg/Bad-Confidence" {
		t.Errorf("rejected = %+v, want [NewOrg/Bad-Confidence]", sum.Rejected)
	}

	// Escalated candidate produced a draft-spec file consumable by `draft`.
	specPath := filepath.Join(specDir, "fresh-coder-32b.spec.json")
	data, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatalf("read escalated spec: %v", err)
	}
	var spec draftSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("escalated spec is not a valid draftSpec: %v", err)
	}
	if spec.ModelID != "fresh-coder-32b" || len(spec.Variants) != 1 {
		t.Errorf("escalated spec wrong: %+v", spec)
	}

	// No spec file for non-escalated candidates.
	if _, err := os.Stat(filepath.Join(specDir, "maybe-coder-7b.spec.json")); !os.IsNotExist(err) {
		t.Error("low-confidence candidate should not produce a spec file")
	}

	// Issue body mentions the candidates and cites sources.
	body, err := os.ReadFile(issueOut)
	if err != nil {
		t.Fatalf("read issue body: %v", err)
	}
	bs := string(body)
	for _, want := range []string{"NewOrg/Fresh-Coder-32B", "74.5", "swebench.com", "draft PR", "Rejected"} {
		if !strings.Contains(bs, want) {
			t.Errorf("issue body missing %q\n%s", want, bs)
		}
	}
}

func TestProposeSubcommand_NoCandidates(t *testing.T) {
	research := writeTemp(t, "empty.json", "[]")
	issueOut := filepath.Join(t.TempDir(), "issue.md")
	if _, err := captureStdout(t, func() error {
		return runPropose([]string{"--research", research, "--issue-out", issueOut})
	}); err != nil {
		t.Fatalf("runPropose empty: %v", err)
	}
	body, _ := os.ReadFile(issueOut)
	if !strings.Contains(string(body), "No new candidate models") {
		t.Errorf("empty radar should say so: %s", body)
	}
}

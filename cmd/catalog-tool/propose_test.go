package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// researchFixture covers each classification path: escalate, report (low
// confidence), report (only a vendor number), report (one score), reject
// (malformed). The accepted source ids here are the ones benchmarks.json
// declares — the fixture would stop escalating if that list changed, which is
// the intended coupling.
const researchFixture = `[
  {
    "repo_id": "NewOrg/Fresh-Coder-32B",
    "scores": {
      "livebench_code_generation": {"value": 74.5, "source": "livebench",
        "url": "https://livebench.ai/table_2026_06_25.csv", "retrieved": "2026-06-18"},
      "vendor_card_humaneval": {"value": 88.0,
        "url": "https://newvendor.example/blog", "retrieved": "2026-06-18"}
    },
    "confidence": "high",
    "license": "apache-2.0",
    "recommended": true,
    "rationale": "Ahead of the 27B we carry on the accepted coding table, at a 24GB-class footprint.",
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
    "repo_id": "NewOrg/Vendor-Only-30B",
    "scores": {
      "vendor_card_swe_bench": {"value": 71.0,
        "url": "https://newvendor.example/paper", "retrieved": "2026-06-18"},
      "vendor_card_humaneval": {"value": 90.0,
        "url": "https://newvendor.example/paper", "retrieved": "2026-06-18"}
    },
    "confidence": "high",
    "recommended": true,
    "rationale": "Strong numbers, but every one of them is the vendor's own.",
    "model": {"model_id": "vendor-only-30b", "context_length": 32768, "variants": []}
  },
  {
    "repo_id": "NewOrg/Maybe-Coder-7B",
    "scores": {
      "livebench_code_generation": {"value": 40.0, "source": "livebench",
        "url": "https://livebench.ai/table_2026_06_25.csv", "retrieved": "2026-06-18"},
      "forum_claim": {"value": 41.0, "url": "https://forum.example/post", "retrieved": "2026-06-18"}
    },
    "confidence": "low",
    "recommended": true,
    "rationale": "Accepted source, but the research is low confidence.",
    "model": {"model_id": "maybe-coder-7b", "context_length": 32768, "variants": []}
  },
  {
    "repo_id": "NewOrg/Thin-Sourced-13B",
    "scores": {
      "livebench_code_generation": {"value": 55.0, "source": "livebench",
        "url": "https://livebench.ai/table_2026_06_25.csv", "retrieved": "2026-06-18"}
    },
    "confidence": "high",
    "recommended": true,
    "rationale": "One score and nothing corroborating it.",
    "model": {"model_id": "thin-13b", "context_length": 32768, "variants": []}
  },
  {
    "repo_id": "NewOrg/Bad-Confidence",
    "scores": {},
    "confidence": "totally",
    "recommended": false,
    "rationale": "Malformed confidence."
  }
]`

func TestProposeSubcommand(t *testing.T) {
	dir := t.TempDir()
	research := writeTemp(t, "research.json", researchFixture)
	specDir := filepath.Join(dir, "specs")
	benchDir := filepath.Join(dir, "bench")
	issueOut := filepath.Join(dir, "issue.md")

	out, err := captureStdout(t, func() error {
		return runPropose([]string{"--research", research, "--spec-dir", specDir,
			"--bench-dir", benchDir, "--issue-out", issueOut})
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
	// Vendor-Only (no accepted source), Maybe-Coder (low confidence) and
	// Thin-Sourced (nothing corroborating) are reported, not escalated.
	if len(sum.Reported) != 3 {
		t.Errorf("reported = %v, want 3 entries", sum.Reported)
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
	for _, want := range []string{"NewOrg/Fresh-Coder-32B", "74.5", "livebench.ai", "draft PR", "Rejected"} {
		if !strings.Contains(bs, want) {
			t.Errorf("issue body missing %q\n%s", want, bs)
		}
	}
	// A vendor-only number is shown, and shown as not usable.
	if !strings.Contains(bs, "no accepted source") {
		t.Errorf("issue body must say which candidates have no accepted source\n%s", bs)
	}
}

// A vendor's own number never auto-drafts, however strong it is. Product
// contract, docs/decisions/20260805/1427-quality-tier-is-a-curated-ladder.md:
// self-reported numbers are not comparable to one another, so they are
// recorded for a human and never acted on by the pipeline.
//
// A/B: the two records differ only in whether the score cites an accepted
// source.
func TestPropose_OnlyAnAcceptedSourceEscalates(t *testing.T) {
	accepted := map[string]bool{"livebench": true}
	base := func(src string) researchRecord {
		return researchRecord{
			RepoID:      "Org/M",
			Confidence:  catalog.ConfidenceHigh,
			Recommended: true,
			Model:       &draftSpec{ModelID: "m"},
			Scores: map[string]catalog.BenchmarkScore{
				"primary":   {Value: 70, Source: src, URL: "u", Retrieved: "2026-06-18"},
				"secondary": {Value: 80, URL: "v", Retrieved: "2026-06-18"},
			},
		}
	}
	if !eligibleForPR(base("livebench"), accepted) {
		t.Error("a score from an accepted source should escalate")
	}
	if eligibleForPR(base(""), accepted) {
		t.Error("a vendor-only number must never auto-draft")
	}
	// And a source we do not accept is rejected outright, not silently ignored.
	if reason := validateResearch(base("some-blog"), accepted); reason == "" {
		t.Error("a score citing an unknown source should be rejected as malformed")
	}
}

// Declared inversion (#523): the old gate required swe_bench_verified > 0, so
// a model that genuinely SCORED zero could not escalate — the same defect that
// made "not found" and "measured zero" indistinguishable in the store. A real
// zero from an accepted source is now escalatable.
func TestPropose_MeasuredZeroCanEscalate(t *testing.T) {
	accepted := map[string]bool{"livebench": true}
	r := researchRecord{
		RepoID: "Org/Zero", Confidence: catalog.ConfidenceHigh, Recommended: true,
		Model: &draftSpec{ModelID: "zero"},
		Scores: map[string]catalog.BenchmarkScore{
			"primary":   {Value: 0, Source: "livebench", URL: "u", Retrieved: "2026-06-18"},
			"secondary": {Value: 1, URL: "v", Retrieved: "2026-06-18"},
		},
	}
	if reason := validateResearch(r, accepted); reason != "" {
		t.Fatalf("a measured zero is valid research: %s", reason)
	}
	if !eligibleForPR(r, accepted) {
		t.Error("a measured zero from an accepted source should escalate; it is a measurement")
	}
}

// The bench-dir output must be something benchmarks.json can accept, or the
// weekly run writes a row the store rejects. Round-trips through the store's
// own type so the two cannot drift.
func TestPropose_BenchDirEntryMatchesStoreShape(t *testing.T) {
	dir := t.TempDir()
	research := writeTemp(t, "research.json", researchFixture)
	if _, err := captureStdout(t, func() error {
		return runPropose([]string{"--research", research, "--bench-dir", dir})
	}); err != nil {
		t.Fatalf("runPropose: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "fresh-coder-32b.bench.json"))
	if err != nil {
		t.Fatalf("read bench entry: %v", err)
	}
	var entry catalog.ModelBenchmarks
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatalf("bench entry is not a ModelBenchmarks: %v\n%s", err, data)
	}
	sc, ok := entry.Scores["livebench_code_generation"]
	if !ok {
		t.Fatalf("bench entry lost the accepted score: %+v", entry)
	}
	if sc.Source != "livebench" || sc.Value != 74.5 || sc.Retrieved == "" {
		t.Errorf("bench entry did not carry provenance: %+v", sc)
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

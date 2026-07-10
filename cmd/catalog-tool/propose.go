package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

func init() {
	subcommands["propose"] = subcommand{run: runPropose, summary: "validate LLM research, render the radar Issue, emit draft-specs"}
}

// researchRecord is the strict schema the headless LLM step must emit per
// candidate. The LLM supplies the FUZZY facts (benchmark scores with cited
// sources, vendor support, model identity); it never supplies VRAM/KV numbers —
// those come from catalog-tool draft/compute. Model mirrors a draft spec so an
// escalated candidate flows straight into `catalog-tool draft`.
type researchRecord struct {
	RepoID           string                    `json:"repo_id"`
	SWEBenchVerified float64                   `json:"swe_bench_verified"`
	Secondary        map[string]float64        `json:"secondary,omitempty"`
	Sources          []catalog.BenchmarkSource `json:"sources"`
	Confidence       string                    `json:"confidence"`
	License          string                    `json:"license"`
	Recommended      bool                      `json:"recommended"`
	Rationale        string                    `json:"rationale"`
	Model            *draftSpec                `json:"model,omitempty"`
}

// proposeSummary is the machine-readable outcome printed to stdout.
type proposeSummary struct {
	Escalated []string         `json:"escalated"` // model_ids that became draft-spec files
	Reported  []string         `json:"reported"`  // repo_ids surfaced in the Issue only
	Rejected  []rejectedRecord `json:"rejected"`  // repo_ids dropped, with reason
}

type rejectedRecord struct {
	RepoID string `json:"repo_id"`
	Reason string `json:"reason"`
}

func runPropose(args []string) error {
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	researchPath := fs.String("research", "", "path to the LLM research JSON array (required)")
	issueOut := fs.String("issue-out", "", "write the rendered radar Issue body (markdown) here")
	specDir := fs.String("spec-dir", "", "directory to write per-escalated-candidate draft-spec JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *researchPath == "" {
		return fmt.Errorf("propose: --research is required")
	}
	raw, err := os.ReadFile(*researchPath)
	if err != nil {
		return fmt.Errorf("propose: read research: %w", err)
	}
	var records []researchRecord
	if err := json.Unmarshal(raw, &records); err != nil {
		return fmt.Errorf("propose: parse research (want a JSON array): %w", err)
	}

	summary := proposeSummary{}
	var escalated []researchRecord
	for _, r := range records {
		if reason := validateResearch(r); reason != "" {
			summary.Rejected = append(summary.Rejected, rejectedRecord{RepoID: r.RepoID, Reason: reason})
			continue
		}
		if eligibleForPR(r) {
			escalated = append(escalated, r)
			summary.Escalated = append(summary.Escalated, r.Model.ModelID)
		} else {
			summary.Reported = append(summary.Reported, r.RepoID)
		}
	}

	if *specDir != "" {
		if err := os.MkdirAll(*specDir, 0o755); err != nil {
			return fmt.Errorf("propose: mkdir spec-dir: %w", err)
		}
		for _, r := range escalated {
			path := filepath.Join(*specDir, r.Model.ModelID+".spec.json")
			data, err := json.MarshalIndent(r.Model, "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
				return fmt.Errorf("propose: write spec %s: %w", path, err)
			}
		}
	}

	if *issueOut != "" {
		body := renderIssueBody(records, summary)
		if err := os.WriteFile(*issueOut, []byte(body), 0o644); err != nil {
			return fmt.Errorf("propose: write issue body: %w", err)
		}
	}

	return printJSON(summary)
}

// validateResearch returns a non-empty rejection reason when a record is
// malformed. It is the backstop a hallucinated record cannot pass.
func validateResearch(r researchRecord) string {
	if r.RepoID == "" {
		return "missing repo_id"
	}
	switch r.Confidence {
	case catalog.ConfidenceHigh, catalog.ConfidenceMedium, catalog.ConfidenceLow:
	default:
		return fmt.Sprintf("invalid confidence %q", r.Confidence)
	}
	if r.SWEBenchVerified < 0 || r.SWEBenchVerified > 100 {
		return fmt.Sprintf("swe_bench_verified %.1f out of [0,100]", r.SWEBenchVerified)
	}
	for _, s := range r.Sources {
		if s.URL == "" || s.Retrieved == "" {
			return "a source is missing url/retrieved"
		}
	}
	if r.Recommended && r.Model == nil {
		return "recommended but no model spec provided"
	}
	return ""
}

// eligibleForPR decides whether a (valid) record escalates to a draft PR. The
// gate is deliberately strict: a benchmark score, >=2 cited sources, medium+
// confidence, and a model spec. Low-confidence or thinly-sourced candidates are
// surfaced in the Issue for a human, never auto-drafted.
func eligibleForPR(r researchRecord) bool {
	if !r.Recommended || r.Model == nil {
		return false
	}
	if r.Confidence != catalog.ConfidenceHigh && r.Confidence != catalog.ConfidenceMedium {
		return false
	}
	if r.SWEBenchVerified <= 0 {
		return false
	}
	if len(r.Sources) < 2 {
		return false
	}
	return r.Model.ModelID != ""
}

// renderIssueBody produces the markdown body for the rolling "Model radar"
// tracking Issue.
func renderIssueBody(records []researchRecord, summary proposeSummary) string {
	var b strings.Builder
	b.WriteString("# Model radar\n\n")
	b.WriteString("_Deterministic HF discovery + LLM-researched benchmarks (issue #413). ")
	b.WriteString("Footprint numbers come from `catalog-tool`; benchmark scores are cited below. ")
	b.WriteString("This issue is refreshed weekly; escalated candidates get a draft PR._\n\n")

	if len(records) == 0 {
		b.WriteString("**No new candidate models this week.**\n")
		return b.String()
	}

	fmt.Fprintf(&b, "**%d candidate(s)** — %d escalated to draft PR, %d reported, %d rejected.\n\n",
		len(records), len(summary.Escalated), len(summary.Reported), len(summary.Rejected))

	b.WriteString("| Model | SWE-bench V | Confidence | Status |\n|---|---|---|---|\n")
	escalatedRepos := escalatedRepoSet(records)
	for _, r := range records {
		status := "reported"
		if escalatedRepos[r.RepoID] {
			status = "✅ draft PR"
		} else if !r.Recommended {
			status = "not recommended"
		}
		swe := "—"
		if r.SWEBenchVerified > 0 {
			swe = fmt.Sprintf("%.1f", r.SWEBenchVerified)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", r.RepoID, swe, r.Confidence, status)
	}
	b.WriteString("\n")

	// Per-candidate detail with sources, so every cited number is reviewable.
	sorted := append([]researchRecord(nil), records...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].SWEBenchVerified > sorted[j].SWEBenchVerified })
	for _, r := range sorted {
		fmt.Fprintf(&b, "### `%s`\n\n", r.RepoID)
		if r.SWEBenchVerified > 0 {
			fmt.Fprintf(&b, "- SWE-bench Verified: **%.1f** (%s confidence)\n", r.SWEBenchVerified, r.Confidence)
		}
		for k, v := range r.Secondary {
			fmt.Fprintf(&b, "- %s: %.1f\n", k, v)
		}
		if len(r.Sources) > 0 {
			b.WriteString("- Sources: ")
			parts := make([]string, 0, len(r.Sources))
			for _, s := range r.Sources {
				parts = append(parts, fmt.Sprintf("%s (%s)", s.URL, s.Retrieved))
			}
			b.WriteString(strings.Join(parts, ", ") + "\n")
		}
		if r.Rationale != "" {
			b.WriteString("- Rationale: " + r.Rationale + "\n")
		}
		b.WriteString("\n")
	}

	if len(summary.Rejected) > 0 {
		b.WriteString("## Rejected (malformed research)\n\n")
		for _, rj := range summary.Rejected {
			fmt.Fprintf(&b, "- `%s`: %s\n", rj.RepoID, rj.Reason)
		}
	}
	return b.String()
}

func escalatedRepoSet(records []researchRecord) map[string]bool {
	set := map[string]bool{}
	for _, r := range records {
		if eligibleForPR(r) && validateResearch(r) == "" {
			set[r.RepoID] = true
		}
	}
	return set
}

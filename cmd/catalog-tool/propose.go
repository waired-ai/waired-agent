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
	RepoID string `json:"repo_id"`

	// Scores mirrors the store's own shape (catalog.ModelBenchmarks.Scores)
	// so the radar and benchmarks.json cannot drift apart. Each score names
	// where it came from; only a score citing an accepted source can send a
	// candidate to a draft PR.
	Scores map[string]catalog.BenchmarkScore `json:"scores,omitempty"`

	Confidence  string     `json:"confidence"`
	License     string     `json:"license"`
	Recommended bool       `json:"recommended"`
	Rationale   string     `json:"rationale"`
	Model       *draftSpec `json:"model,omitempty"`
}

// acceptedFrom returns the source ids a score may cite, read from the store
// rather than written here so changing the accepted set is a one-file edit.
func acceptedFrom(bs catalog.BenchmarkSet) map[string]bool {
	out := map[string]bool{}
	for _, a := range bs.AcceptedSources {
		out[a.ID] = true
	}
	return out
}

// bestAccepted returns the highest score citing an accepted source, and
// whether there was one. It is the sort key for the Issue and the escalation
// gate's input.
func (r researchRecord) bestAccepted(accepted map[string]bool) (float64, bool) {
	best, found := 0.0, false
	for _, sc := range r.Scores {
		if sc.Source == "" || !accepted[sc.Source] {
			continue
		}
		if !found || sc.Value > best {
			best, found = sc.Value, true
		}
	}
	return best, found
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
	benchDir := fs.String("bench-dir", "", "directory to write per-escalated-candidate benchmarks.json entries")
	if err := fs.Parse(args); err != nil {
		return err
	}
	store, err := catalog.Benchmarks()
	if err != nil {
		return fmt.Errorf("propose: read the benchmark store: %w", err)
	}
	accepted := acceptedFrom(store)
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
		if reason := validateResearch(r, accepted); reason != "" {
			summary.Rejected = append(summary.Rejected, rejectedRecord{RepoID: r.RepoID, Reason: reason})
			continue
		}
		if eligibleForPR(r, accepted) {
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

	if *benchDir != "" {
		if err := os.MkdirAll(*benchDir, 0o755); err != nil {
			return fmt.Errorf("propose: mkdir bench-dir: %w", err)
		}
		for _, r := range escalated {
			path := filepath.Join(*benchDir, r.Model.ModelID+".bench.json")
			data, err := json.MarshalIndent(benchEntry(r), "", "  ")
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
				return fmt.Errorf("propose: write bench entry %s: %w", path, err)
			}
		}
	}

	if *issueOut != "" {
		body := renderIssueBody(records, summary, accepted)
		if err := os.WriteFile(*issueOut, []byte(body), 0o644); err != nil {
			return fmt.Errorf("propose: write issue body: %w", err)
		}
	}

	return printJSON(summary)
}

// validateResearch returns a non-empty rejection reason when a record is
// malformed. It is the backstop a hallucinated record cannot pass.
func validateResearch(r researchRecord, accepted map[string]bool) string {
	if r.RepoID == "" {
		return "missing repo_id"
	}
	switch r.Confidence {
	case catalog.ConfidenceHigh, catalog.ConfidenceMedium, catalog.ConfidenceLow:
	default:
		return fmt.Sprintf("invalid confidence %q", r.Confidence)
	}
	for _, key := range sortedScoreKeys(r.Scores) {
		sc := r.Scores[key]
		if sc.Value < 0 || sc.Value > 100 {
			return fmt.Sprintf("score %q = %.1f, out of [0,100]", key, sc.Value)
		}
		if sc.URL == "" || sc.Retrieved == "" {
			return fmt.Sprintf("score %q is missing url/retrieved", key)
		}
		if sc.Source != "" && !accepted[sc.Source] {
			return fmt.Sprintf("score %q cites source %q, which is not in the accepted list", key, sc.Source)
		}
	}
	if r.Recommended && r.Model == nil {
		return "recommended but no model spec provided"
	}
	return ""
}

func sortedScoreKeys(m map[string]catalog.BenchmarkScore) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// eligibleForPR decides whether a (valid) record escalates to a draft PR. The
// gate stays deliberately strict: medium+ confidence, a model spec, at least
// one score from an ACCEPTED source, and at least one more score corroborating
// it. Everything else is surfaced in the Issue for a human, never auto-drafted.
//
// Two changes from the pre-2026-08-05 gate, both stated in the PR that made
// them (#523):
//
//   - It used to require swe_bench_verified > 0, so a model that genuinely
//     SCORED zero could not escalate. The store could not tell that from "not
//     found" either; both defects go together.
//   - "at least two sources" used to mean two rows in a flat Sources list.
//     Under one accepted source per benchmark that phrasing is incoherent, so
//     it now means the accepted score plus one corroborating score — which is
//     the same strength, with the vendor card in its correct role.
func eligibleForPR(r researchRecord, accepted map[string]bool) bool {
	if !r.Recommended || r.Model == nil {
		return false
	}
	if r.Confidence != catalog.ConfidenceHigh && r.Confidence != catalog.ConfidenceMedium {
		return false
	}
	if _, ok := r.bestAccepted(accepted); !ok {
		return false
	}
	if len(r.Scores) < 2 {
		return false
	}
	return r.Model.ModelID != ""
}

// renderIssueBody produces the markdown body for the rolling "Model radar"
// tracking Issue.
func renderIssueBody(records []researchRecord, summary proposeSummary, accepted map[string]bool) string {
	var b strings.Builder
	b.WriteString("# Model radar\n\n")
	b.WriteString("_Deterministic HF discovery + LLM-researched benchmarks (waired-ai/waired#413). ")
	b.WriteString("Footprint numbers come from `catalog-tool`; benchmark scores are cited below. ")
	b.WriteString("Only a score from an accepted source can escalate a candidate — everything else is ")
	b.WriteString("recorded for a human to read. This issue is refreshed weekly._\n\n")

	if len(records) == 0 {
		b.WriteString("**No new candidate models this week.**\n")
		return b.String()
	}

	fmt.Fprintf(&b, "**%d candidate(s)** — %d escalated to draft PR, %d reported, %d rejected.\n\n",
		len(records), len(summary.Escalated), len(summary.Reported), len(summary.Rejected))

	b.WriteString("| Model | Best accepted score | Scores | Confidence | Status |\n|---|---|---|---|---|\n")
	escalatedRepos := escalatedRepoSet(records, accepted)
	for _, r := range records {
		status := "reported"
		switch {
		case escalatedRepos[r.RepoID]:
			status = "✅ draft PR"
		case !r.Recommended:
			status = "not recommended"
		}
		best := "—"
		if v, ok := r.bestAccepted(accepted); ok {
			best = fmt.Sprintf("%.1f", v)
		} else if len(r.Scores) > 0 {
			best = "— (no accepted source)"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %d | %s | %s |\n", r.RepoID, best, len(r.Scores), r.Confidence, status)
	}
	b.WriteString("\n")

	// Per-candidate detail with sources, so every cited number is reviewable.
	sorted := append([]researchRecord(nil), records...)
	sort.SliceStable(sorted, func(i, j int) bool {
		a, aok := sorted[i].bestAccepted(accepted)
		c, cok := sorted[j].bestAccepted(accepted)
		if aok != cok {
			return aok // records with an accepted score sort first
		}
		return a > c
	})
	for _, r := range sorted {
		fmt.Fprintf(&b, "### `%s`\n\n", r.RepoID)
		fmt.Fprintf(&b, "- Confidence: %s\n", r.Confidence)
		for _, k := range sortedScoreKeys(r.Scores) {
			sc := r.Scores[k]
			origin := "recorded only, no accepted source"
			if sc.Source != "" && accepted[sc.Source] {
				origin = "**" + sc.Source + "**"
			} else if sc.Source != "" {
				origin = sc.Source + " (not accepted)"
			}
			fmt.Fprintf(&b, "- %s: %.1f — %s, %s (%s)\n", k, sc.Value, origin, sc.URL, sc.Retrieved)
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

func escalatedRepoSet(records []researchRecord, accepted map[string]bool) map[string]bool {
	set := map[string]bool{}
	for _, r := range records {
		if validateResearch(r, accepted) == "" && eligibleForPR(r, accepted) {
			set[r.RepoID] = true
		}
	}
	return set
}

// benchEntry renders the ready-to-merge benchmarks.json row for one candidate.
// Emitting it from Go rather than assembling it in jq inside open-draft-pr.sh
// means the store's type is the only place the shape lives -- and that shell
// is never exercised by PR CI, so drift there is invisible until the weekly
// run.
func benchEntry(r researchRecord) catalog.ModelBenchmarks {
	return catalog.ModelBenchmarks{Scores: r.Scores}
}

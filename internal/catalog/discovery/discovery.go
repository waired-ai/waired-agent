// Package discovery is the pure (no-I/O) filter + diff logic of the catalog
// radar: given a list of Hugging Face models and the current catalog/seen
// state, it decides which are fresh candidates worth researching. Keeping it
// I/O-free makes the filter rules trivially testable against recorded fixtures.
package discovery

import (
	"slices"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog/hfclient"
)

// Candidate is a model that passed every cheap deterministic filter and is
// worth the (expensive) benchmark research step.
type Candidate struct {
	RepoID    string `json:"repo_id"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	License   string `json:"license"`
	WhyPassed string `json:"why_passed"`
	ConfigURL string `json:"config_url"`
}

// FilterOpts carries the deterministic gates. Time is injected (CreatedAfter)
// so the package stays pure.
type FilterOpts struct {
	AllowedLicenses []string  // lower-case SPDX ids, e.g. {"apache-2.0","mit"}
	CreatedAfter    time.Time // recency window lower bound
	// Known is the set of repo_ids / aliases already in the catalog
	// (lower-cased). A candidate already represented is dropped.
	Known map[string]bool
	// Ledger tracks previously-seen repos; flagged/dismissed are skipped.
	Ledger Ledger
	// HFBaseURL is used only to build the config.json URL in the output.
	HFBaseURL string
}

// Filter applies the deterministic gates and returns the surviving candidates.
// The gates (in order): license allow-list, text-generation capability, not
// gated, created within the window, not already in the catalog, not a
// flagged/dismissed repo in the ledger.
func Filter(models []hfclient.HubModel, opts FilterOpts) []Candidate {
	allowed := map[string]bool{}
	for _, l := range opts.AllowedLicenses {
		allowed[strings.ToLower(l)] = true
	}
	var out []Candidate
	for _, m := range models {
		repo := m.RepoID()
		if repo == "" {
			continue
		}
		if len(allowed) > 0 && !allowed[m.License()] {
			continue
		}
		if !isTextGeneration(m) {
			continue
		}
		if m.IsGated() {
			continue
		}
		if !opts.CreatedAfter.IsZero() {
			t, err := time.Parse(time.RFC3339, m.CreatedAt)
			if err != nil || t.Before(opts.CreatedAfter) {
				continue
			}
		}
		if opts.Known[strings.ToLower(repo)] {
			continue
		}
		if opts.Ledger.ShouldSkip(repo) {
			continue
		}
		out = append(out, Candidate{
			RepoID:    repo,
			Author:    authorOf(repo),
			CreatedAt: m.CreatedAt,
			License:   m.License(),
			WhyPassed: whyPassed(m),
			ConfigURL: configURL(opts.HFBaseURL, repo),
		})
	}
	return out
}

func isTextGeneration(m hfclient.HubModel) bool {
	return m.PipelineTag == "text-generation" || slices.Contains(m.Tags, "text-generation")
}

func authorOf(repo string) string {
	if i := strings.IndexByte(repo, '/'); i > 0 {
		return repo[:i]
	}
	return ""
}

func whyPassed(m hfclient.HubModel) string {
	return "license=" + m.License() + ", text-generation, not gated, recent, not in catalog"
}

func configURL(base, repo string) string {
	if base == "" {
		base = hfclient.DefaultBaseURL
	}
	return base + "/" + repo + "/resolve/main/config.json"
}

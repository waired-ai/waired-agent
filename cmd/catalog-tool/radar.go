package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/catalog/discovery"
	"github.com/waired-ai/waired-agent/internal/catalog/hfclient"
)

func init() {
	subcommands["radar"] = subcommand{run: runRadar, summary: "poll HuggingFace for new candidate models"}
}

// defaultRadarOrgs are the upstream orgs whose open coding models we track.
// The runbook grows this list; the license filter already excludes non-OSS.
var defaultRadarOrgs = []string{"Qwen", "openai", "zai-org", "deepseek-ai", "mistralai"}

type radarOutput struct {
	Orgs       []string              `json:"orgs"`
	SinceDays  int                   `json:"since_days"`
	Candidates []discovery.Candidate `json:"candidates"`
}

func runRadar(args []string) error {
	fs := flag.NewFlagSet("radar", flag.ContinueOnError)
	orgs := fs.String("orgs", strings.Join(defaultRadarOrgs, ","), "comma-separated HuggingFace orgs to scan")
	sinceDays := fs.Int("since-days", 60, "only consider models created within this many days")
	limit := fs.Int("limit", 50, "max models to fetch per org (newest first)")
	licenses := fs.String("licenses", "apache-2.0,mit", "comma-separated allowed licenses")
	ledgerPath := fs.String("ledger", "internal/catalog/discovery/seen.json", "path to the seen-ledger JSON")
	record := fs.Bool("record", false, "append new candidates to the ledger as 'candidate' and write it back")
	nowRFC := fs.String("now", "", "RFC3339 timestamp for ledger writes (default: current time)")
	hfBaseURL := fs.String("hf-base-url", hfclient.DefaultBaseURL, "HuggingFace base URL (override for testing)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	now := time.Now().UTC()
	if *nowRFC != "" {
		t, err := time.Parse(time.RFC3339, *nowRFC)
		if err != nil {
			return fmt.Errorf("radar: bad --now: %w", err)
		}
		now = t
	}
	createdAfter := now.AddDate(0, 0, -*sinceDays)

	known, err := knownCatalogRepos()
	if err != nil {
		return err
	}

	ledger, err := loadLedger(*ledgerPath)
	if err != nil {
		return err
	}

	client := &hfclient.Client{BaseURL: *hfBaseURL, HTTP: hfclient.New().HTTP}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	orgList := splitCSV(*orgs)
	var all []hfclient.HubModel
	for _, org := range orgList {
		models, err := client.ListModels(ctx, org, *limit)
		if err != nil {
			return fmt.Errorf("radar: list %s: %w", org, err)
		}
		all = append(all, models...)
	}

	cands := discovery.Filter(all, discovery.FilterOpts{
		AllowedLicenses: splitCSV(*licenses),
		CreatedAfter:    createdAfter,
		Known:           known,
		Ledger:          ledger,
		HFBaseURL:       *hfBaseURL,
	})

	if *record && len(cands) > 0 {
		nowStr := now.Format(time.RFC3339)
		for _, c := range cands {
			ledger.Record(c.RepoID, discovery.StatusCandidate, nowStr)
		}
		out, err := ledger.Marshal()
		if err != nil {
			return fmt.Errorf("radar: marshal ledger: %w", err)
		}
		if err := os.WriteFile(*ledgerPath, out, 0o644); err != nil {
			return fmt.Errorf("radar: write ledger %s: %w", *ledgerPath, err)
		}
	}

	return printJSON(radarOutput{Orgs: orgList, SinceDays: *sinceDays, Candidates: cands})
}

// knownCatalogRepos collects the repo_ids / HF-style aliases already present in
// the bundled catalog (lower-cased) so the radar never re-proposes them.
func knownCatalogRepos() (map[string]bool, error) {
	ms, err := catalog.BundledManifests()
	if err != nil {
		return nil, fmt.Errorf("radar: load bundled catalog: %w", err)
	}
	known := map[string]bool{}
	for _, m := range ms {
		known[strings.ToLower(m.ModelID)] = true
		for _, a := range m.ModelAliases {
			if strings.Contains(a, "/") {
				known[strings.ToLower(a)] = true
			}
		}
		for _, v := range m.Variants {
			if v.Source.RepoID != "" {
				known[strings.ToLower(v.Source.RepoID)] = true
			}
		}
	}
	return known, nil
}

func loadLedger(path string) (discovery.Ledger, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return discovery.LoadLedger(nil)
	}
	if err != nil {
		return discovery.Ledger{}, fmt.Errorf("radar: read ledger %s: %w", path, err)
	}
	return discovery.LoadLedger(data)
}

func splitCSV(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

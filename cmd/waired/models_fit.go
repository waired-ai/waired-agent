package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// confirmModelFitsForPull gates `waired models pull` on the host meeting
// the model's recommended spec (#61). Waired's policy is warn-but-allow:
// an over-spec pick is never silently blocked, but the user must
// explicitly confirm it — interactively (default No) or, in a
// non-interactive context, by passing --yes. The fit verdict comes from
// the agent's /inference/catalog endpoint (the same UMA-aware fit logic
// the tray and `models ls --detail` use), so the CLI never re-derives it.
//
// Returns (proceed, err). Fail-open: if the catalog can't be fetched or
// the model can't be matched to a family, proceed is true — a
// confirmation gate must never turn an infra hiccup into a hard failure.
// A non-nil err means the pull must be aborted (non-interactive over-spec
// pick without --yes); the caller surfaces it verbatim.
func confirmModelFitsForPull(mgmt, model string, assumeYes bool, out io.Writer, in io.Reader) (bool, error) {
	fam, ok := lookupCatalogFamily(mgmt, model)
	if !ok || fam.Fits {
		return true, nil // unknown fit, or it fits → no gate
	}

	name := fam.DisplayName
	if name == "" {
		name = model
	}
	deficit := fam.DeficitLabel
	if deficit == "" {
		deficit = "exceeds this host's recommended spec"
	}
	writePromptf(out, "\n%s %s exceeds this host's recommended spec: %s.\n", emo("⚠", "!"), name, deficit)
	writePrompt(out, "  It will still be pulled and can run, but may be slow or fail to load.")

	if assumeYes {
		writePrompt(out, "  Proceeding (--yes).")
		return true, nil
	}
	if !stdinIsInteractive() {
		return false, fmt.Errorf("%s exceeds this host's recommended spec (%s); re-run with --yes to pull it anyway", model, deficit)
	}
	return ynPrompt(out, bufio.NewScanner(in), "Pull it anyway?", false), nil
}

// lookupCatalogFamily fetches /inference/catalog and returns the family
// matching model (by model_id, else by trailing path segment for short
// forms). ok=false on any fetch/decode error or no match — callers treat
// that as "fit unknown" and fail open. Alias forms the catalog response
// does not carry (e.g. waired/moe-coding) fall through to ok=false rather
// than risk matching the wrong family.
func lookupCatalogFamily(mgmt, model string) (catalogDetailFamily, bool) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(mgmt + "/waired/v1/inference/catalog")
	if err != nil {
		return catalogDetailFamily{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return catalogDetailFamily{}, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return catalogDetailFamily{}, false
	}
	var cat catalogDetailResp
	if err := json.Unmarshal(body, &cat); err != nil {
		return catalogDetailFamily{}, false
	}
	for _, f := range cat.Families {
		if strings.EqualFold(f.ModelID, model) {
			return f, true
		}
	}
	// Short form: the arg may be a bare id or an alias whose trailing
	// segment matches a model_id (the catalog keys on model_id).
	seg := model
	if i := strings.LastIndex(model, "/"); i >= 0 {
		seg = model[i+1:]
	}
	if seg != model {
		for _, f := range cat.Families {
			if strings.EqualFold(f.ModelID, seg) {
				return f, true
			}
		}
	}
	return catalogDetailFamily{}, false
}

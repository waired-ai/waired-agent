package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// confirmModelFitsForPull gates `waired models pull` (#61). Two
// different things, and the difference is the whole point:
//
//   - fits=false means this computer does not have the memory — weights
//     plus the window's KV cache plus engine overhead exceed RAM and
//     graphics memory together. Loading it is a certain OOM, so the pull
//     is REFUSED with the shortfall. This is the only gate the ratified
//     policy allows to refuse anything (waired-ai/waired#1056,
//     2026-08-03 owner decision; the arithmetic is #464's).
//   - not_recommended means it runs here and Waired would pick something
//     else. Warned, then honoured — interactively (default No) or with
//     --yes in a non-interactive context.
//
// The two used to be one gate that --yes could clear, so the CLI pulled
// models the browser hard-disabled and docs-site said were refused
// (#465 item 4).
//
// The fit verdict comes from the agent's /inference/catalog endpoint (the
// same fit logic the tray and `models ls --detail` use), so the CLI never
// re-derives it.
//
// Returns (proceed, err). Fail-open: if the catalog can't be fetched or
// the model can't be matched to a family, proceed is true — a gate must
// never turn an infra hiccup into a hard failure. A non-nil err means the
// pull must be aborted; the caller surfaces it verbatim.
func confirmModelFitsForPull(mgmt, model string, assumeYes bool, out io.Writer, in io.Reader) (bool, error) {
	fam, ok := lookupCatalogFamily(mgmt, model)
	if !ok {
		return true, nil // unknown fit → no gate
	}
	name := fam.DisplayName
	if name == "" {
		name = model
	}

	if !fam.Fits {
		deficit := fam.DeficitLabel
		if deficit == "" {
			deficit = "there is not enough memory on this computer"
		}
		writePromptf(out, "\n%s %s does not fit in this computer's memory: %s.\n",
			emo("⚠", "!"), name, deficit)
		writePrompt(out, "  Run `waired models ls --detail` to see what does fit.")
		// No --yes escape on purpose: the flag skips a confirmation, and
		// this is not one. Nothing downstream can recover from weights
		// that do not fit, so "pull it anyway" would only spend the
		// download and fail at load.
		return false, fmt.Errorf("%s does not fit in this computer's memory (%s)", model, deficit)
	}

	// It runs. It may still be the wrong choice for this machine
	// (waired-ai/waired#988), and until waired-agent#321 nothing said so
	// on any surface. Warned, then honoured.
	if fam.Fit == nil || !fam.Fit.NotRecommended {
		return true, nil
	}
	writePromptf(out, "\n%s %s runs on this computer, but Waired would not choose it here%s.\n",
		emo("ℹ", "i"), name, notRecommendedBecause(fam.Fit.NotRecommendedReason))
	if assumeYes {
		return true, nil
	}
	if !stdinIsInteractive() {
		// Non-interactive stays permissive: this is a demotion, and
		// failing a scripted pull over one would break setups that work
		// today for a reason the operator can neither see nor answer.
		return true, nil
	}
	return ynPrompt(out, bufio.NewScanner(in), "Use it anyway?", false), nil
}

// notRecommendedBecause turns the demotion code into the clause that
// completes "Waired would not choose it here…". Unknown codes yield no
// clause rather than a guess — the sentence is already true without one,
// and the vocabulary is allowed to grow.
func notRecommendedBecause(reason string) string {
	switch reason {
	case hostfit.ReasonWeightsSpill:
		return ": it does not fit entirely on the graphics card, and every reply pays for that"
	case hostfit.ReasonTooSlow:
		return ": replies would be slow"
	case hostfit.ReasonWindowTooSmall:
		// The only one that is not about this computer. No machine makes
		// this model hold a coding session, so naming hardware would send
		// someone shopping for something that cannot help.
		return ": it cannot hold a long coding session — a coding agent has to compact " +
			"much earlier with it, and will lose the start of the work if it does not"
	case hostfit.ReasonWindowExceedsMemory:
		return ": this computer cannot hold a long coding session with it, though it answers well otherwise"
	}
	return ""
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

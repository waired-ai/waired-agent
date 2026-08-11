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

// confirmModelFitsForPull gates `waired models pull` (#61, #583). Two
// different warnings, and the difference is what consent takes:
//
//   - fits=false means this computer does not have the memory — weights
//     plus the window's KV cache plus engine overhead exceed RAM and
//     graphics memory together, so loading it is expected to fail after
//     the download is spent. It is still a choice
//     (waired-ai/waired#1067, 2026-08-08 owner decision: no surface
//     refuses a model any more; supersedes waired-ai/waired#1056's
//     refusal rule): interactively it asks with the shortfall, default
//     No, and a script consents with --yes --force. --yes alone
//     deliberately does not cover it — that flag skips confirmations
//     whose safe answer is yes, and this one's is No.
//   - not_recommended means it runs here and Waired would pick something
//     else. Warned, then honoured — interactively (default No) or with
//     --yes in a non-interactive context.
//
// The fit verdict comes from the agent's /inference/catalog endpoint (the
// same fit logic the tray and `models ls --detail` use), so the CLI never
// re-derives it.
//
// Returns (proceed, err). Fail-open: if the catalog can't be fetched or
// the model can't be matched to a family, proceed is true — a gate must
// never turn an infra hiccup into a hard failure. A decline is
// (false, nil) — a choice, not a fault — which the caller reports as a
// cancelled pull. A non-nil err means the pull must be aborted; the
// caller surfaces it verbatim.
func confirmModelFitsForPull(mgmt, model string, assumeYes, force bool, out io.Writer, in io.Reader) (bool, error) {
	fam, host, ok := lookupCatalogFamily(mgmt, model)
	if !ok {
		return true, nil // unknown fit → no gate
	}
	name := fam.DisplayName
	if name == "" {
		name = model
	}

	if !fam.Fits {
		warnModelDoesNotFitOn(out, name, fam.DeficitLabel, host)
		switch unfitPullAction(assumeYes, force, stdinIsInteractive()) {
		case pullProceed:
			return true, nil
		case pullDecline:
			writePrompt(out, "  Not downloading. Re-run with --yes --force to download it anyway.")
			return false, nil
		}
		return ynPrompt(out, bufio.NewScanner(in), "Download it anyway?", false), nil
	}

	// It runs, and this is where the download is still avoidable, so say
	// what it will cost to run (#632). Stated, not asked: the
	// recommendation rules are not changing here, and a second
	// default-No prompt on a model the catalog recommends would be a
	// demotion by the back door — the ratified place for speed to
	// exclude is a MEASURED input (waired-agent#466,
	// docs/decisions/20260804/1937-… decision 4).
	if s := contextCacheSpillNote(host, fam.Fit); s != "" {
		writePromptf(out, "\n%s %s %s\n", emo("ℹ", "i"), name, s)
	}

	// It runs. It may still be the wrong choice for this machine
	// (waired-ai/waired#988), and until waired-agent#321 nothing said so
	// on any surface. Warned, then honoured.
	if fam.Fit == nil || !fam.Fit.NotRecommended {
		return true, nil
	}
	warnModelNotRecommended(out, name, fam.Fit.NotRecommendedReason)
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

// warnModelDoesNotFit prints the does-not-fit warning (#592's confirmed
// copy). One function, two surfaces — `models pull` and the init model
// picker (#586) — so the wording cannot drift between them.
func warnModelDoesNotFit(out io.Writer, name, deficit string) {
	warnModelDoesNotFitOn(out, name, deficit, catalogDetailHost{})
}

// warnModelDoesNotFitOn is warnModelDoesNotFit with the host block in
// hand, so the warning can say what the "allocatable" figure is made of.
//
// The deficit line quotes the verdict's own two numbers and stops there
// (#625). This adds the one sentence that makes the smaller of them
// checkable: a 16 GB Mac told a model needs 11 GB and it has 6 is
// reading a true sentence with no way to see where the other 10 GB
// went, and that missing figure is the install-time measurement #568
// exists to take.
func warnModelDoesNotFitOn(out io.Writer, name, deficit string, host catalogDetailHost) {
	if deficit == "" {
		deficit = "there is not enough memory on this computer"
	}
	writePromptf(out, "\n%s %s does not fit in this computer's memory: %s.\n",
		emo("⚠", "!"), name, deficit)
	if s := hostMemoryBreakdown(host); s != "" {
		writePromptf(out, "  %s\n", s)
	}
	writePrompt(out, "  Loading it is expected to fail after the download completes.")
	writePrompt(out, "  Run `waired models ls --detail` to see what does fit.")
}

// hostMemoryBreakdown words what this computer has and what is already
// spoken for, or "" when there is nothing worth saying.
//
// Silent on two hosts, both deliberately. One that reported no RAM at
// all has no figures to break down. One whose reservation is still the
// flat hostfit.OSMemoryAllowanceGB floor has not measured anything —
// printing a constant as though it were an observation about this
// machine is how a number stops being trusted.
func hostMemoryBreakdown(host catalogDetailHost) string {
	if host.RAMTotalGB <= 0 || host.OSReservedGB <= hostfit.OSMemoryAllowanceGB {
		return ""
	}
	// Graphics memory is named separately only where it IS separate. On
	// a unified-memory host the same bytes back both figures, and adding
	// them in a sentence would count them twice
	// (docs/decisions/20260804/1937-…, the provenance split).
	has := fmt.Sprintf("%d GB", host.RAMTotalGB)
	if !host.UnifiedMemory && host.VRAMTotalMB > 0 {
		has = fmt.Sprintf("%d GB RAM + %d GB graphics memory",
			host.RAMTotalGB, (host.VRAMTotalMB+1023)/1024)
	}
	return fmt.Sprintf("This computer has %s; %d GB is already in use by the system and other apps.",
		has, host.OSReservedGB)
}

// contextCacheSpillMB is how much of a full coding session's working set
// this host cannot keep on the graphics card: what the model needs to
// serve the coding window, less the memory the engine may address there.
//
// 0 for a row that fits on the card, and equally for a host with no card
// and for an agent too old to report a budget. All three are "nothing to
// say" rather than "nothing spills", which is why the caller prints
// nothing instead of "0 GB".
func contextCacheSpillMB(host catalogDetailHost, fit *catalogDetailFit) int {
	if fit == nil || host.GPUBudgetMB <= 0 || fit.RequiredWindowResidentMB <= 0 {
		return 0
	}
	if over := fit.RequiredWindowResidentMB - host.GPUBudgetMB; over > 0 {
		return over
	}
	return 0
}

// contextCacheSpillNote words that shortfall for a surface with a line
// to spare, or "" when there is none.
//
// It reports a FACT about memory, deliberately not a speed. The
// prediction that would have named the rc8 Windows host's 5 tok/s before
// its 6.6 GB download is a measured input this catalog does not carry
// (waired-agent#466); what it does carry is the arithmetic an operator
// can check — 10719 MB to serve the window, 8188 MB of card.
func contextCacheSpillNote(host catalogDetailHost, fit *catalogDetailFit) string {
	mb := contextCacheSpillMB(host, fit)
	if mb <= 0 {
		return ""
	}
	return fmt.Sprintf("runs here, but about %s of its context cache will not fit on the graphics card and is read from system RAM instead.",
		formatSpillGB(mb))
}

// formatSpillGB writes a shortfall in GB, with one decimal below 10 GB
// so a 2531 MB gap does not round to a flat "3 GB" the operator cannot
// reconcile with the two figures it came from.
func formatSpillGB(mb int) string {
	gb := float64(mb) / 1024
	if gb < 10 {
		return fmt.Sprintf("%.1f GB", gb)
	}
	return fmt.Sprintf("%.0f GB", gb)
}

// warnModelNotRecommended prints the runs-but-demoted warning
// (waired-agent#321), shared with the picker for the same reason.
func warnModelNotRecommended(out io.Writer, name, reason string) {
	writePromptf(out, "\n%s %s runs on this computer, but is not recommended here%s.\n",
		emo("ℹ", "i"), name, notRecommendedBecause(reason))
}

// pullFitAction is what the does-not-fit branch does for one flag/tty
// combination.
type pullFitAction int

const (
	pullProceed pullFitAction = iota
	pullDecline
	pullAsk
)

// unfitPullAction decides how a fits=false pull is confirmed
// (waired-ai/waired#1067, 2026-08-08 owner decision). Auto-consent
// takes BOTH --yes and --force; anything less asks a present human and
// declines an absent one. --force without --yes still asks: the pair is
// the scripted consent, not two synonyms.
func unfitPullAction(assumeYes, force, interactive bool) pullFitAction {
	switch {
	case assumeYes && force:
		return pullProceed
	case interactive:
		return pullAsk
	default:
		return pullDecline
	}
}

// notRecommendedBecause turns the demotion code into the clause that
// completes "is not recommended here…" (subjectless since waired#1146,
// owner-approved 2026-08-12). Unknown codes yield no
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
// does not carry (e.g. a name from a newer catalog) fall through to ok=false rather
// than risk matching the wrong family.
// The host block rides along because the warning explains the verdict
// with this host's own figures (#625) and re-fetching the catalog to get
// them would let the two halves of one sentence come from two reads.
func lookupCatalogFamily(mgmt, model string) (catalogDetailFamily, catalogDetailHost, bool) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(mgmt + "/waired/v1/inference/catalog")
	if err != nil {
		return catalogDetailFamily{}, catalogDetailHost{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return catalogDetailFamily{}, catalogDetailHost{}, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return catalogDetailFamily{}, catalogDetailHost{}, false
	}
	var cat catalogDetailResp
	if err := json.Unmarshal(body, &cat); err != nil {
		return catalogDetailFamily{}, catalogDetailHost{}, false
	}
	for _, f := range cat.Families {
		if strings.EqualFold(f.ModelID, model) {
			return f, cat.Host, true
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
				return f, cat.Host, true
			}
		}
	}
	return catalogDetailFamily{}, catalogDetailHost{}, false
}

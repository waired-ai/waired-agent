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
		warnModelWillNotRun(out, name, fam, host)
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

// warnModelWillNotRun prints the warning for a model this host is not
// expected to run, choosing the shape from the VERDICT rather than from
// the rendered deficit string.
//
// Four shapes, because the walls take different actions. A memory
// shortfall is answered by choosing a smaller model; an engine below the
// variant's floor is answered by updating the engine, and the model is
// the right one; a model this way of running AI has no build of is
// answered by neither, on any hardware. Until waired-agent#836 there was
// one shape, so a 121 GB host was told "does not fit in this computer's
// memory: needs ollama ≥ 0.32.13" with its memory broken down underneath
// and "see what does fit" as the remedy — four sentences about the wrong
// wall. The tray had the same defect from the other end
// (waired-agent#850).
//
// MEMORY IS THE ALLOWLIST, not the fall-through. That inversion is the
// whole of waired-agent#862: with one special case in front of it, every
// verdict that was not that case inherited a paragraph asserting a cause
// nobody had checked — a 16 GB host switching to a vLLM-only family read
// "does not fit in this computer's memory: no variant supports ollama",
// with a memory breakdown and a download that does not exist, for a
// verdict taken before the capacity check ever ran
// (internal/router.FamilyBestFit returns as soon as no variant is
// loadable). A code this binary has not learned yet now lands on the
// neutral arm, which repeats what the row said and claims nothing more.
// Ruled in docs/decisions/20260819/1910-an-engine-floor-degrades-with-a-reason.md,
// which names `waired models pull` / `waired init` as the CLI half.
//
// hostfit.ReasonNoGPU is deliberately NOT a capacity code here — the
// tray's allowlist is the same three. It is reached only by a
// vLLM-serving host with no card at all, where the wall is the absent
// card and a RAM breakdown points at the wrong hardware to go buy; its
// deficit label ("needs 24 GB VRAM (no GPU)") already says it, so the
// neutral arm is both true and enough.
//
// Keyed on Fit.Reason, never on the prose: deciding a warning's shape by
// matching a display string authored in another package is what made
// #850 reachable. An older agent's wire carries no code at all, and
// keeps the memory arm exactly as it did before #836 — its DeficitLabel
// is the only thing it ever had. Deliberately not the neutral arm: that
// one exists for a reason code newer than this binary, which is a live
// case, while a nil Fit is a frozen one that no longer ships.
func warnModelWillNotRun(out io.Writer, name string, fam catalogDetailFamily, host catalogDetailHost) {
	fit := fam.Fit
	if fit == nil {
		warnModelDoesNotFit(out, name, fam.DeficitLabel, host)
		return
	}
	switch fit.Reason {
	case reasonEngineTooOld:
		warnEngineTooOld(out, name, fam.DeficitLabel, fit.HaveEngineVersion)
	case reasonNoVariantForEngine:
		warnNoBuildForEngine(out, name, fam.DeficitLabel)
	case reasonInsufficientMemory, reasonInsufficientRAM, reasonInsufficientVRAM:
		warnModelDoesNotFit(out, name, fam.DeficitLabel, host)
	default:
		warnModelWillNotRunHere(out, name, fam.DeficitLabel)
	}
}

// warnNoBuildForEngine words the verdict that is about the CATALOG, not
// about this computer: nothing here can serve this model, and no
// hardware would change that.
//
// It takes no deficit label on purpose. The router's label for this
// verdict is "no variant supports ollama"
// (internal/router.FamilyBestFit) — two words of ours and an engine
// name, for a person who has never heard of either — and
// docs/decisions/20260819/1910-… item 3 keeps engine internal names out
// of user copy. `models ls --detail` and the picker override it for the
// same reason; this is the surface that was pasting it into a sentence
// about memory instead.
//
// No memory breakdown and no "see what does fit": both would answer a
// question this verdict never asked.
func warnNoBuildForEngine(out io.Writer, name, deficit string) {
	if deficit == "" {
		deficit = "no variant for this engine"
	}
	writePromptf(out, "\n%s %s has %s, so the inference engine on this computer cannot run it.\n",
		emo("⚠", "!"), name, deficit)
	writePrompt(out, "  Downloading it now is expected to fail.")
	writePrompt(out, "  Run `waired models ls --detail` to see what does run here.")
}

// warnModelWillNotRunHere is the arm for a verdict this binary does not
// recognise — a reason code added to proto/hostfit after this CLI
// shipped, or one it declines to classify (ReasonNoGPU today).
//
// It repeats the row's own sentence and asserts nothing else. Echoing
// what the operator already saw is both true and the least surprising
// thing to print, and it is what the tray settled on for the same case
// (waired-agent#850, unfitSwitchPrompt's default arm). An empty label
// drops the clause rather than filling it in: "will not run on this
// computer" with no reason is a smaller lie than a reason we made up.
func warnModelWillNotRunHere(out io.Writer, name, deficit string) {
	if deficit == "" {
		writePromptf(out, "\n%s %s won't run on this computer.\n", emo("⚠", "!"), name)
	} else {
		writePromptf(out, "\n%s %s won't run on this computer: %s.\n",
			emo("⚠", "!"), name, deficit)
	}
	writePrompt(out, "  Downloading it now is expected to fail.")
	writePrompt(out, "  Run `waired models ls --detail` to see what does run here.")
}

// warnEngineTooOld words the engine-version floor for a terminal.
//
// It deliberately does not claim the model FITS. This branch is decided
// before the capacity check runs (internal/router.FamilyBestFit returns
// as soon as no variant is loadable), so a model can be both too big and
// too new for the engine here, and "it fits, just update" would be a
// sentence we have not checked. What it says instead is why no memory
// figure is printed beside it.
//
// have is "" when nothing could read the engine's version. That is a
// different situation with a different first step — the engine may be
// installed and merely never started (waired-agent#836) — so it gets its
// own sentence rather than the word "unknown" dropped into this one.
func warnEngineTooOld(out io.Writer, name, deficit, have string) {
	if have == "" {
		writePromptf(out, "\n%s %s %s.\n", emo("⚠", "!"), name, deficit)
		writePrompt(out, "  Downloading it now is expected to fail.")
		writePrompt(out, "  Run `waired runtimes ls` to see the engine this computer has, then `waired update` to bring it up to date.")
	} else {
		writePromptf(out, "\n%s %s %s.\n", emo("⚠", "!"), name, deficit)
		writePrompt(out, "  Downloading it now is expected to fail.")
		writePrompt(out, "  Waired updates the engine for you: run `waired update`, then try again.")
	}
	writePrompt(out, "  This isn't a memory shortfall. Whether it fits here is decided once the engine can load it.")
}

// warnModelDoesNotFit prints the does-not-fit warning (#592's confirmed
// copy). One function for every surface that words a capacity refusal,
// so the wording cannot drift between them.
//
// The deficit line quotes the verdict's own two numbers and stops there
// (#625). The host block adds the one sentence that makes the smaller of
// them checkable: a 16 GB Mac told a model needs 11 GB and it has 6 is
// reading a true sentence with no way to see where the other 10 GB
// went, and that missing figure is the install-time measurement #568
// exists to take. A caller with no host block passes the zero value and
// the sentence is dropped rather than half-written.
//
// Reached only through warnModelWillNotRun's capacity arm. There is no
// shorter overload to call instead: the one that existed took no verdict
// and went straight to this paragraph, which is the entry point
// waired-agent#862 is about — a surface that reaches for it words a
// no-build or engine-floor refusal as a memory shortfall.
func warnModelDoesNotFit(out io.Writer, name, deficit string, host catalogDetailHost) {
	if deficit == "" {
		deficit = "there isn't enough memory on this computer"
	}
	writePromptf(out, "\n%s %s doesn't fit in this computer's memory: %s.\n",
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
		has = fmt.Sprintf("%d GB RAM + %d GB VRAM",
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
	return fmt.Sprintf("runs here, but about %s of its KV cache won't fit in VRAM and is read from system RAM instead.",
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
	writePromptf(out, "\n%s %s runs on this computer, but isn't recommended here%s.\n",
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
		return ": it doesn't fit entirely in VRAM, and every reply pays for that"
	case hostfit.ReasonTooSlow:
		return ": replies would be slow"
	case hostfit.ReasonWindowTooSmall:
		// The only one that is not about this computer. No machine makes
		// this model hold a coding session, so naming hardware would send
		// someone shopping for something that cannot help.
		return ": it can't hold a long coding session, so a coding agent has to compact " +
			"much earlier with it and loses the start of the work if it doesn't"
	case hostfit.ReasonWindowExceedsMemory:
		return ": this computer can't hold a long coding session with it, though it answers well otherwise"
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

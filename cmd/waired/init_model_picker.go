package main

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// Owner-ruled install-flow model step (2026-08-08, waired-ai/waired#1067;
// waired-agent#586): the terminal init path used to apply the daemon's
// model selection silently. Now it lists the catalog with the
// recommended pick preselected, re-checks the fit live on confirm (the
// #592 warn-then-honour confirms, shared verbatim with `models pull`),
// and offers "don't download a model now" — a NORMAL completed state:
// the engine stays installed, exit stays 0, and a model can be added
// later via the browser dashboard or `waired models pull`.
//
// The picker only runs while this host has no model history at all — no
// active model, nothing downloaded or downloading, no stored preference.
// Re-running `waired init` on a configured host re-asks nothing yet; the
// owner-ruled full re-run (engine re-ask, re-measure, resident-memory
// deduction, and what "no model" means beside a serving one) is
// waired-agent#599.

// modelPickerOutcome is how the picker ended. The zero value means it
// did not answer — skipped, declined by an error, or taken over — and
// the daemon's own selection stands, exactly as before this step
// existed.
type modelPickerOutcome struct {
	// none is the operator's "don't download a model now": the caller
	// skips the model wait, because nothing is coming and that is fine.
	none bool
	// picked is the model_id the operator chose ("" when none/skip).
	picked string
}

// runInitModelPicker runs the step. sc is the init stdin owner;
// stillMine reports whether this terminal still owns the questions
// (§4.2, the confirmHostSpeedBudget contract).
//
// Every early return withdraws the pending-question claim the engine
// step registered with the daemon (postModelChoicePending), so the held
// fallback download proceeds as if the question had never been coming.
// An ANSWER — a model or none — is its own withdrawal on the daemon
// side, so answered paths skip the explicit one.
func runInitModelPicker(mgmtURL string, nonInteractive bool, pinnedModelID string, sc lineReader, out io.Writer, stillMine func() bool) modelPickerOutcome {
	answered := false
	defer func() {
		if !answered {
			postModelChoicePending(mgmtURL, false)
		}
	}()

	// The same precedence the engine ask uses: an explicit pin is the
	// answer (warn-then-honour happens at apply time), and
	// --non-interactive keeps the daemon's auto-selection.
	if nonInteractive || pinnedModelID != "" || !stillMine() {
		return modelPickerOutcome{}
	}
	cat, ok := fetchCatalogDetail(mgmtURL)
	if !ok || len(cat.Families) == 0 {
		// An older daemon or an unreachable catalog: fail open to the
		// pre-#586 behaviour rather than inventing an empty question.
		return modelPickerOutcome{}
	}
	if hostHasModelHistory(cat) {
		return modelPickerOutcome{}
	}

	def := renderModelPickerList(out, cat)
	for {
		choice, eof := readModelChoice(sc, out, def, len(cat.Families))
		if choice == 0 {
			if err := postPreferredNone(mgmtURL); err != nil {
				writePromptf(out, "warn: could not record the choice (%v); the agent keeps its own selection\n", err)
				return modelPickerOutcome{}
			}
			answered = true
			writePrompt(out, "No model selected — the AI software stays ready.")
			writePrompt(out, "Pick one later with `waired models pull <model>` or from the browser dashboard.")
			return modelPickerOutcome{none: true}
		}
		f := cat.Families[choice-1]
		// The live re-check: hardware headroom moves (an engine loaded,
		// a VM started), and the verdict shown in the list is minutes
		// old by the time a human confirms.
		if fresh, ok := fetchCatalogDetail(mgmtURL); ok {
			if ff, found := familyByID(fresh, f.ModelID); found {
				f = ff
			}
		}
		name := modelPickerName(f)
		if !f.Fits {
			// The zero host is what this surface has always passed, so
			// the memory arm prints exactly what it printed before
			// (#568's breakdown stays a `models pull` line). Only the
			// engine-floor arm is new here — waired-agent#836.
			warnModelWillNotRun(out, name, f, catalogDetailHost{})
			if !ynPrompt(out, sc, "Download it anyway?", false) {
				if eof {
					// Nothing more is coming from stdin; looping would
					// re-read the same silence forever. The daemon's own
					// selection stands.
					return modelPickerOutcome{}
				}
				renderModelPickerList(out, cat) // No returns to the list
				continue
			}
		} else if f.Fit != nil && f.Fit.NotRecommended {
			warnModelNotRecommended(out, name, f.Fit.NotRecommendedReason)
			if !ynPrompt(out, sc, "Use it anyway?", false) {
				if eof {
					return modelPickerOutcome{}
				}
				renderModelPickerList(out, cat)
				continue
			}
		}
		if err := postPreferredModel(mgmtURL, f.ModelID); err != nil {
			writePromptf(out, "warn: could not apply the model choice (%v); the agent keeps its own selection\n", err)
			return modelPickerOutcome{}
		}
		answered = true
		return modelPickerOutcome{picked: f.ModelID}
	}
}

// hostHasModelHistory reports whether this host has already decided
// anything about models — an answered model question, an active model,
// weights on disk, or a download in flight. The picker is the FIRST
// choice only (#586); re-choosing on a configured host is
// waired-agent#599.
//
// "Decided" means a PERSON decided, and until waired-agent#627 the first
// test could not tell: it read a non-empty preferred_model_id as proof
// someone had chosen, when the setup reconciler writes that same field
// when it applies an instruction from the control plane. On a real first
// install that removed the picker outright — a step an owner ruling put
// there (waired-ai/waired#1067, 2026-08-08) — because a preference
// landed mid-init five minutes before the picker would have run. So the
// question asked here is now the one that was always meant: has the model
// question been ANSWERED on this host? The daemon answers it from the
// preference's recorded provenance.
//
// The per-family `preferred` flag goes with it, and for the same
// reason: the daemon computes it as "this family's id equals the stored
// preference", so it is the SAME claim projected onto a row and carries
// the same defect. It is not lost — a preference a person set answers
// the question above, which is checked first.
//
// The weights-based signals stay as they were, because a model that is
// active or on disk means this host is past its first choice however it
// got there. The one they have to tell apart is
// hostfit.HostCutoffProbeModelID: #496 measures the host by pulling it,
// through the same registry the catalog reports downloads from, and it
// is a real catalog entry rather than a private fixture — so its weights
// arriving used to read as history and skip the picker entirely
// (waired-agent#607). Not a race: step 6 blocks until the measurement
// lands, and the measurement cannot land until that pull has finished,
// so on the ordinary interactive path the picker was unreachable.
// Weights Waired fetched to measure with are not an answer; being ACTIVE
// or PREFERRED still is, because that model is a legitimate pick
// (quality_tier 12, the smallest offered entry).
//
// Same exclusion, one layer down, in cmd/waired-agent/setup_desired.go:
// the probe model does not trigger ensureHostSpeedMeasured either.
func hostHasModelHistory(cat catalogDetailResp) bool {
	if cat.ModelQuestionAnswered {
		return true
	}
	for _, f := range cat.Families {
		if f.Active {
			return true
		}
		if f.ModelID == hostfit.HostCutoffProbeModelID {
			continue
		}
		if f.Downloaded || f.Downloading {
			return true
		}
	}
	return false
}

// renderModelPickerList prints the numbered catalog (approved copy,
// waired-agent#586) and returns the default choice: the recommended
// row's number, or 0 — "don't download a model now" — on a host where
// nothing is recommended (the auto-selection would pick nothing there
// either, waired-ai/waired#1067 R5).
func renderModelPickerList(out io.Writer, cat catalogDetailResp) (def int) {
	// The FIRST marked row, matching what step 4 used to read
	// (waired-agent#649). The daemon marks exactly one — pinned by
	// TestInferenceCatalog_MarksExactlyOneRecommendedPick — so this
	// changes no shipped behaviour; it removes a way for two surfaces
	// reading the same catalog to disagree if that ever slips.
	for i, f := range cat.Families {
		if f.RecommendedPick {
			def = i + 1
			break
		}
	}
	writePrompt(out)
	if def > 0 {
		writePrompt(out, "Choose the AI model for this computer (Enter = recommended):")
	} else {
		writePrompt(out, "Choose the AI model for this computer:")
	}
	// The engine question is asked EARLIER in this wizard, so reaching
	// this list with no engine is a choice already made, not something
	// to re-offer. What the rows do need is the context that they are
	// about a computer that will not run them itself — without it the
	// list reads as a menu of what is about to start running here
	// (#852). nil means a daemon predating the field: say nothing.
	if cat.EngineInstalled != nil && !*cat.EngineInstalled {
		writePrompt(out)
		writePrompt(out, "No AI engine is installed on this computer, so it will not run a model")
		writePrompt(out, "itself — requests go to your other computers. Your choice here is the")
		writePrompt(out, "model this computer would run if you add an engine later.")
	}
	writePrompt(out)
	for i, f := range cat.Families {
		writePromptf(out, "  %d) %s\n", i+1, modelPickerRow(cat.Host, f))
	}
	writePrompt(out, "  0) Don't download a model now")
	writePrompt(out)
	return def
}

// modelPickerName is how a model is written to a person: the catalog's
// display name, falling back to the id for an entry that has none.
//
// It exists so every surface that names a model in the install flow
// spells it the same way. Step 4 used to print the display name while
// these rows printed bare ids, so the same model read as two different
// things within one wizard run (waired-agent#649).
func modelPickerName(f catalogDetailFamily) string {
	if f.DisplayName != "" {
		return f.DisplayName
	}
	return f.ModelID
}

// modelPickerRow is one list line: the model's name, plus the verdict a
// person needs before picking it. Unfit rows carry the same deficit
// prose `waired models ls --detail` prints, so the two surfaces agree.
//
// A fitting row also carries what the graphics card cannot hold of a
// full coding session (waired-agent#632). It is the same clause, from
// the same two helpers, that the FIT column of `models ls --detail`
// prints — the picker is where the choice is actually made, and a cost
// only visible on a surface the operator has to go looking for is a cost
// they learn about afterwards.
//
// It states a memory FACT, not a speed. What a spilling row costs in
// tok/s is a measurement this catalog does not carry
// (waired-agent#466), and the recommendation is deliberately unchanged
// by it: excluding on predicted speed was ruled out on 2026-08-04
// (decision 4) and stays out until a measured input exists.
func modelPickerRow(host catalogDetailHost, f catalogDetailFamily) string {
	name := modelPickerName(f)
	switch {
	case f.RecommendedPick:
		return name + " — recommended for this computer" + pickerSpillSuffix(host, f)
	case !f.Fits:
		if f.Fit != nil && f.Fit.Reason == reasonNoVariantForEngine {
			return name + " — not available on this computer"
		}
		if f.DeficitLabel != "" {
			return name + " — " + f.DeficitLabel
		}
		return name + " — does not fit in this computer's memory"
	default:
		return name + pickerSpillSuffix(host, f)
	}
}

// pickerSpillSuffix is the context-cache clause, or "" when this host
// has nothing to say about residency — a row that fits on the card, a
// machine with no card, or a daemon too old to report a budget. All
// three print nothing rather than "0 GB", which would read as a measured
// zero.
//
// Byte-identical to the `models ls --detail` clause on purpose: two
// spellings of one fact is the defect waired-agent#649 fixed on the
// recommendation, and this is the same pair of surfaces.
func pickerSpillSuffix(host catalogDetailHost, f catalogDetailFamily) string {
	mb := contextCacheSpillMB(host, f.Fit)
	if mb <= 0 {
		return ""
	}
	return " · " + formatSpillGB(mb) + " of context cache in system RAM"
}

// readModelChoice reads one numbered answer. Empty input takes def;
// unparseable input re-prompts up to 3 times then falls back to def
// (the ynPrompt convention). eof reports that stdin is exhausted, so
// callers must not loop back to another read.
func readModelChoice(sc lineReader, out io.Writer, def, n int) (choice int, eof bool) {
	for range 3 {
		writePromptf(out, "Model [%d]: ", def)
		if !sc.Scan() {
			return def, true
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			return def, false
		}
		if v, err := strconv.Atoi(line); err == nil && v >= 0 && v <= n {
			return v, false
		}
		writePromptf(out, "  please answer 0-%d.\n", n)
	}
	return def, false
}

// fetchCatalogDetail GETs /inference/catalog; ok is false on any
// transport/decode error so callers fail open.
func fetchCatalogDetail(mgmtURL string) (catalogDetailResp, bool) {
	body, err := httpGet(mgmtURL + "/waired/v1/inference/catalog")
	if err != nil {
		return catalogDetailResp{}, false
	}
	var cat catalogDetailResp
	if err := json.Unmarshal(body, &cat); err != nil {
		return catalogDetailResp{}, false
	}
	return cat, true
}

func familyByID(cat catalogDetailResp, modelID string) (catalogDetailFamily, bool) {
	for _, f := range cat.Families {
		if f.ModelID == modelID {
			return f, true
		}
	}
	return catalogDetailFamily{}, false
}

// postPreferredModel applies the choice through the same endpoint the
// tray and wizard use, so the daemon owns every consequence (pull,
// activation, fallback stand-down).
func postPreferredModel(mgmtURL, modelID string) error {
	body, err := json.Marshal(struct {
		ModelID string `json:"model_id"`
	}{modelID})
	if err != nil {
		return err
	}
	_, err = httpPost(mgmtURL+"/waired/v1/inference/preferred-model", body)
	return err
}

// postPreferredNone records "don't download a model now" (#586).
func postPreferredNone(mgmtURL string) error {
	_, err := httpPost(mgmtURL+"/waired/v1/inference/preferred-model", []byte(`{"none":true}`))
	return err
}

// postModelChoicePending is `waired init` telling the daemon the model
// question is coming (true) or no longer coming (false), so the bundled
// fallback download waits for the answer instead of racing it
// (waired-agent#586). Best-effort by design: an older daemon 404s, and
// the daemon bounds the claim server-side either way.
func postModelChoicePending(mgmtURL string, pending bool) {
	body, err := json.Marshal(struct {
		Pending bool `json:"pending"`
	}{pending})
	if err != nil {
		return
	}
	_, _ = httpPost(mgmtURL+"/waired/v1/inference/model-choice-pending", body)
}

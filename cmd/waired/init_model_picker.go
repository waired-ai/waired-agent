package main

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
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
		name := f.DisplayName
		if name == "" {
			name = f.ModelID
		}
		if !f.Fits {
			warnModelDoesNotFit(out, name, f.DeficitLabel)
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
// anything about models — an active or preferred model, weights on disk,
// or a download in flight. The picker is the FIRST choice only (#586);
// re-choosing on a configured host is waired-agent#599.
func hostHasModelHistory(cat catalogDetailResp) bool {
	if cat.PreferredModelID != "" {
		return true
	}
	for _, f := range cat.Families {
		if f.Active || f.Preferred || f.Downloaded || f.Downloading {
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
	for i, f := range cat.Families {
		if f.RecommendedPick {
			def = i + 1
		}
	}
	writePrompt(out)
	if def > 0 {
		writePrompt(out, "Choose the AI model for this computer (Enter = recommended):")
	} else {
		writePrompt(out, "Choose the AI model for this computer:")
	}
	writePrompt(out)
	for i, f := range cat.Families {
		writePromptf(out, "  %d) %s\n", i+1, modelPickerRow(f))
	}
	writePrompt(out, "  0) Don't download a model now")
	writePrompt(out)
	return def
}

// modelPickerRow is one list line: the model id, plus the verdict a
// person needs before picking it. Unfit rows carry the same deficit
// prose `waired models ls --detail` prints, so the two surfaces agree.
func modelPickerRow(f catalogDetailFamily) string {
	switch {
	case f.RecommendedPick:
		return f.ModelID + " — recommended for this computer"
	case !f.Fits:
		if f.Fit != nil && f.Fit.Reason == reasonNoVariantForEngine {
			return f.ModelID + " — not available on this computer"
		}
		if f.DeficitLabel != "" {
			return f.ModelID + " — " + f.DeficitLabel
		}
		return f.ModelID + " — does not fit in this computer's memory"
	default:
		return f.ModelID
	}
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

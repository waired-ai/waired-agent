package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// newModelsUseCmd sets the model this computer runs.
//
// Until now nothing on the CLI could. `models pull` and `models rm` move
// weights on and off the disk; which model the device SERVES was settable
// only by re-running `waired init`, from the tray, or from the browser
// dashboard — and the dashboard's model card is gated on the control
// plane believing setup finished, which is exactly what a host that never
// opens a browser could not make it believe (waired-agent#753). A machine
// installed by a provisioning script therefore had no way at all to change
// its model, from anywhere.
//
// The name was cited before it existed: two remediation lines used to
// point at `waired models use`, which was one of the commands
// waired-agent#465 found being recommended and not shipped. This is that
// command, so those lines are now honest.
//
// It posts to the same endpoint the tray, the wizard and the install
// picker use, so the daemon owns every consequence — the preference file,
// the pull, the activation, the fallback stand-down — and an operator at
// this machine is exactly who that endpoint's Source: operator record
// means. Since waired-agent#812 the switch applies in process, so the
// usual answer involves no restart at all.
func newModelsUseCmd() *cobra.Command {
	var mgmt string
	var assumeYes bool
	var force bool
	var wait bool
	var window string
	cmd := &cobra.Command{
		Use:   "use <model_id|alias>",
		Short: "Set the model this computer runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			model := args[0]
			win, err := parseWindowFlag(window)
			if err != nil {
				return err
			}
			// #61/#583: warn-then-honour, the same gate `models pull`
			// applies. Switching to a model this host cannot hold is the
			// same mistake as downloading one, one step further along.
			proceed, err := confirmModelFitsForPull(mgmt, model, assumeYes, force, stdout, os.Stdin)
			if err != nil {
				return err
			}
			if !proceed {
				fmt.Fprintln(stdout, "switch cancelled.")
				return nil
			}
			// A build this computer has already failed to load is its own
			// question, asked before the window one and with the same
			// default: No. It is NOT a refusal — the owner's ruling of
			// 2026-09-20 is that an explicit choice is warned about and
			// then honoured — and answering yes clears the record on the
			// daemon so the switch actually loads (waired-agent#1453).
			if !assumeYes {
				if why := didNotLoadHere(mgmt, model); why != "" {
					warnDidNotLoadHere(stdout, model, why)
					if ynAsk(stdout, bufio.NewScanner(os.Stdin),
						"Choose it anyway?", false) != ynYes {
						fmt.Fprintln(stdout, "switch cancelled.")
						return nil
					}
				}
			}
			// The long window is its own question, asked after the fit one
			// and with the same default: No. It is not a capacity matter —
			// a computer that can hold it is still being told what the
			// extension costs (owner ruling 2026-09-20,
			// waired-ai/waired#1456).
			if win == hostfit.ServingWindow1M && !assumeYes {
				warnLongContextWindow(stdout, model)
				if ynAsk(stdout, bufio.NewScanner(os.Stdin),
					"Use the 1M context window on this computer?", false) != ynYes {
					fmt.Fprintln(stdout, "switch cancelled.")
					return nil
				}
			}

			body, err := httpPost(mgmt+"/waired/v1/inference/preferred-model",
				mustMarshalPreferredModel(model, win))
			if err != nil {
				if msg, handled := formatModelsUseError(mgmt, model, err); handled {
					fmt.Fprintln(stdout, msg)
					return errModelsUseRefused
				}
				return err
			}

			var res modelsUseResult
			if err := json.Unmarshal(body, &res); err != nil {
				return fmt.Errorf("decode: %w", err)
			}
			if res.ModelID == "" {
				res.ModelID = model
			}
			fmt.Fprintln(stdout, formatModelsUse(res))

			if !wait || !res.Downloading {
				return nil
			}
			return waitForModelReady(mgmt, res.ModelID, 30*time.Minute)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the over-spec confirmation prompt")
	cmd.Flags().BoolVar(&force, "force", false,
		"with --yes, also confirm switching to a model that doesn't fit in this computer's memory")
	// Default off, unlike `models pull --wait`. The old model usually keeps
	// answering for the whole download, so there is nothing being blocked
	// on here — and a provisioning script that ran this would otherwise
	// sit for tens of minutes it never asked for. When nothing answers in
	// the meantime, the confirmation says so (waired-agent#1515).
	cmd.Flags().BoolVar(&wait, "wait", false, "poll until the new model is ready to serve")
	cmd.Flags().StringVar(&window, "window", "200k",
		"context window to serve: 200k, or 1m where the model documents a way past its trained length")
	return cmd
}

// errModelsUseRefused marks a switch the daemon declined for a reason
// already printed in full. Returning it keeps the exit status non-zero
// for scripts without cobra printing a second, worse sentence underneath
// the one the operator is meant to read.
var errModelsUseRefused = errors.New("")

func mustMarshalPreferredModel(modelID string, contextWindow int) []byte {
	b, _ := json.Marshal(struct {
		ModelID       string `json:"model_id"`
		ContextWindow int    `json:"context_window,omitempty"`
	}{modelID, contextWindow})
	return b
}

// parseWindowFlag turns --window into a serving window. The two spellings are
// the two the product uses in its own copy.
//
// The coding window is 0 and not ServingWindow200k, because 0 is what the
// coding window is called everywhere this value travels — the preference
// file, the wire field, the sizing input — and because an omitted field
// leaves the request byte-identical to the one a daemon that predates the
// choice has always received.
//
// An unknown spelling is an error rather than a fall back to the coding
// window: someone who typed a window meant one, and serving the other without
// saying so is the failure the window contract exists to remove.
func parseWindowFlag(v string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "200k":
		return 0, nil
	case "1m":
		return hostfit.ServingWindow1M, nil
	}
	return 0, fmt.Errorf("--window must be 200k or 1m, got %q", v)
}

// warnLongContextWindow is the warn half of the warn-and-ask a person goes
// through to pick the long window. Owner-approved copy, 2026-09-21.
//
// It states the two costs and says plainly which parts are unmeasured. The
// order is deliberate: what the long window DOES to the model first, then
// what it costs in time, then the question — the shape every other prompt in
// this file takes.
func warnLongContextWindow(out io.Writer, name string) {
	writePromptf(out, "\n%s 1M runs %s past the 262,144 tokens it was trained for.\n",
		emo("⚠", "!"), name)
	writePromptf(out, "  The extension applies to every request, so short prompts may be\n")
	writePromptf(out, "  affected. Neither that nor how well it recalls text past 262,144\n")
	writePromptf(out, "  tokens has been measured.\n")
	writePromptf(out, "  Re-reading a long session can take hours, which is what happens when\n")
	writePromptf(out, "  a conversation branches.\n")
}

// modelsUseResult is the daemon's answer to `/preferred-model`
// (management.PreferredModelResponse). A daemon that predates the fields
// after Downloading sends none of them, and the answer is then worded as it
// always was.
type modelsUseResult struct {
	ModelID        string `json:"model_id"`
	WillRestart    bool   `json:"will_restart"`
	Downloading    bool   `json:"downloading"`
	EngineRestarts bool   `json:"engine_restarts"`
	NothingAnswers bool   `json:"nothing_answers"`
	NeedVRAMMB     int    `json:"need_vram_mb"`
	HaveVRAMMB     int    `json:"have_vram_mb"`
}

// formatModelsUse renders the daemon's answer. Pure, so the wording is
// testable without a daemon (formatModelsCancel's shape).
//
// Owner-approved copy (waired-agent#1515, 2026-09-22). Each sentence is the
// one that is true for what the switch actually does: a vLLM engine
// restarts to load the new model, and while it downloads something may or
// may not be answering.
func formatModelsUse(r modelsUseResult) string {
	id := r.ModelID
	var msg string
	switch {
	case r.WillRestart:
		msg = id + " is recorded as the model this computer runs. The background service restarts to apply it."
	case r.Downloading && r.NothingAnswers:
		msg = id + " will run on this computer once it finishes downloading.\n" +
			"Nothing answers on this computer until then."
	case r.Downloading && r.EngineRestarts:
		msg = id + " will run on this computer once it finishes downloading.\n" +
			"The current model keeps answering until then. The engine then restarts to load " + id +
			", and this computer doesn't answer until it's ready."
	case r.Downloading:
		msg = id + " will run on this computer once it finishes downloading.\n" +
			"The current model keeps answering until then."
	case r.EngineRestarts:
		msg = id + " will run on this computer once the engine restarts to load it.\n" +
			"This computer doesn't answer until it's ready."
	default:
		msg = id + " is now the model this computer runs."
	}
	// A choice this computer is not expected to hold is still honoured
	// (owner ruling, 2026-09-20); the first sentence says what will be
	// attempted, and this one what to expect of it.
	if line := formatNotExpectedToFit(id, r.NeedVRAMMB, r.HaveVRAMMB); line != "" {
		msg += "\n" + line
	}
	return msg
}

// formatNotExpectedToFit is the line for a build this computer is not
// expected to start: its catalog minimum against this computer's vLLM VRAM
// budget, rounded the way the model catalog's row rounds them (the need up,
// the have down). "" when the figures do not say so. Owner-approved copy
// (waired-agent#1515, 2026-09-22).
func formatNotExpectedToFit(modelID string, needMB, haveMB int) string {
	if needMB <= 0 || haveMB <= 0 || needMB <= haveMB {
		return ""
	}
	return fmt.Sprintf("%s needs %d GB of VRAM (have %d GB), so it isn't expected to start here.",
		modelID, (needMB+1023)/1024, haveMB/1024)
}

// formatModelsUseError turns the refusals this endpoint has words for
// into the sentence the operator needs, and reports whether it did.
// Anything else is returned to the caller unchanged: an error this build
// has no reading of is better shown raw than paraphrased.
//
// The refusals are all 409, so the machine-readable code — not the
// status — is what tells them apart.
func formatModelsUseError(mgmt, requested string, err error) (string, bool) {
	var me *mgmtStatusError
	if !errors.As(err, &me) {
		return "", false
	}
	parsed := parseMgmtError(me.StatusCode, []byte(me.Message))
	switch {
	case parsed.StatusCode == http.StatusNotFound && catalog.IsCustomModelID(requested):
		// A custom id this computer does not hold is either not here yet —
		// imported a moment ago and the list has not reached this computer
		// — or deleted in the console (waired-ai/waired#1480).
		return "This computer has no custom model " + requested + ". If it was imported just now, " +
			"wait a minute for it to reach this computer and try again; if it was deleted, import it " +
			"again in the Waired console's Custom models tab.", true
	case parsed.StatusCode == http.StatusNotFound:
		return "No model with that name. Run `waired models ls` to see what this computer can run.", true
	case parsed.Code == "custom_model_wrong_engine":
		// The daemon's sentence names the model, both engines and what to
		// choose; nothing was recorded (waired-ai/waired#1480).
		return parsed.Message, true
	case parsed.Code == "model_retired":
		// The daemon names the successor, and it is the only party that
		// knows it. Its sentence verbatim rather than a rewrite that
		// could name a different model than the one it resolved (#200).
		return parsed.Message, true
	case parsed.Code == "window_not_reachable":
		// The daemon's sentence names the model's own length, which is the
		// fact the person needs and the only party holding the manifest
		// knows. Printed verbatim for the same reason model_retired is:
		// a rewrite here could name a different number than the one the
		// handler checked.
		return parsed.Message + "\nPick a model that documents a longer window, or drop --window.", true
	case parsed.Code == "model_switch_unavailable":
		// The choice is KEPT on the daemon side and applies by itself
		// once pulls work again, so this is not "nothing happened" — and
		// saying it had switched, which this answered before the swap
		// layer reported the refusal at all, is waired-agent#257.
		if serving := servingModelID(mgmt); serving != "" {
			return "Couldn't download the weights for " + requested +
				", so this computer keeps running " + serving + ".\n" +
				"The choice is recorded and applies once downloads work again.", true
		}
		// Nothing to name: say the same thing without the clause rather
		// than guess at what is serving.
		return "Couldn't download the weights for " + requested + ".\n" +
			"The choice is recorded and applies once downloads work again.", true
	}
	return "", false
}

// servingModelID is the model this computer is running right now, or ""
// when that cannot be established. Only consulted on the refusal path, so
// a second round-trip costs nothing in the ordinary case.
func servingModelID(mgmt string) string {
	cat, ok := fetchCatalogDetail(mgmt)
	if !ok {
		return ""
	}
	for _, f := range cat.Families {
		if f.Active {
			return f.ModelID
		}
	}
	return ""
}

// didNotLoadHere asks the daemon whether this computer has already failed to
// load a build, and why.
//
// Fails open, like every other reader of fetchCatalogDetail: a daemon that
// cannot answer leaves the switch alone. The question is a courtesy before a
// choice, never a gate on it — the ruling is that an explicit choice is
// honoured, so an unreachable daemon must not be able to block one.
func didNotLoadHere(mgmt, modelID string) string {
	cat, ok := fetchCatalogDetail(mgmt)
	if !ok {
		return ""
	}
	f, ok := familyByID(cat, modelID)
	if !ok {
		return ""
	}
	return f.DidNotLoadHere
}

// warnDidNotLoadHere is the ratified wording (owner, 2026-09-21).
//
// Past tense throughout: this computer TRIED this build and the load failed,
// so nothing here is a prediction. The heading states the state rather than
// asking, and only the last line is a question — ynAsk adds the
// "[y/N] (default: No)".
func warnDidNotLoadHere(out io.Writer, model, why string) {
	fmt.Fprintf(out, "\n%s %s did not load on this computer\n", emo("\u26a0", "!"), model)
	fmt.Fprint(out, "  Waired tried this build before and ran out of memory loading it. The\n")
	fmt.Fprint(out, "  engine stopped and no Waired row on this computer answered. Choosing it\n")
	fmt.Fprint(out, "  again runs the same load: minutes of work, and the computer back under\n")
	fmt.Fprint(out, "  the memory pressure it just came out of.\n")
	fmt.Fprintf(out, "  (%s)\n\n", why)
}

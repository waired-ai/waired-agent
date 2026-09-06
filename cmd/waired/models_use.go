package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"
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
	cmd := &cobra.Command{
		Use:   "use <model_id|alias>",
		Short: "Set the model this computer runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			model := args[0]
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

			body, err := httpPost(mgmt+"/waired/v1/inference/preferred-model",
				mustMarshalPreferredModel(model))
			if err != nil {
				if msg, handled := formatModelsUseError(mgmt, model, err); handled {
					fmt.Fprintln(stdout, msg)
					return errModelsUseRefused
				}
				return err
			}

			var res struct {
				ModelID     string `json:"model_id"`
				WillRestart bool   `json:"will_restart"`
				Downloading bool   `json:"downloading"`
			}
			if err := json.Unmarshal(body, &res); err != nil {
				return fmt.Errorf("decode: %w", err)
			}
			if res.ModelID == "" {
				res.ModelID = model
			}
			fmt.Fprintln(stdout, formatModelsUse(res.ModelID, res.WillRestart, res.Downloading))

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
	// Default off, unlike `models pull --wait`. The old model keeps
	// answering for the whole download, so there is nothing being blocked
	// on here — and a provisioning script that ran this would otherwise
	// sit for tens of minutes it never asked for.
	cmd.Flags().BoolVar(&wait, "wait", false, "poll until the new model is ready to serve")
	return cmd
}

// errModelsUseRefused marks a switch the daemon declined for a reason
// already printed in full. Returning it keeps the exit status non-zero
// for scripts without cobra printing a second, worse sentence underneath
// the one the operator is meant to read.
var errModelsUseRefused = errors.New("")

func mustMarshalPreferredModel(modelID string) []byte {
	b, _ := json.Marshal(struct {
		ModelID string `json:"model_id"`
	}{modelID})
	return b
}

// formatModelsUse renders the daemon's answer. Pure, so the wording is
// testable without a daemon (formatModelsCancel's shape).
func formatModelsUse(modelID string, willRestart, downloading bool) string {
	switch {
	case willRestart:
		return modelID + " is recorded as the model this computer runs. The background service restarts to apply it."
	case downloading:
		return modelID + " will run on this computer once it finishes downloading.\n" +
			"The current model keeps answering until then."
	default:
		return modelID + " is now the model this computer runs."
	}
}

// formatModelsUseError turns the two refusals this endpoint has words for
// into the sentence the operator needs, and reports whether it did.
// Anything else is returned to the caller unchanged: an error this build
// has no reading of is better shown raw than paraphrased.
//
// Both refusals are 409, so the machine-readable code — not the status —
// is what tells them apart.
func formatModelsUseError(mgmt, requested string, err error) (string, bool) {
	var me *mgmtStatusError
	if !errors.As(err, &me) {
		return "", false
	}
	parsed := parseMgmtError(me.StatusCode, []byte(me.Message))
	switch {
	case parsed.StatusCode == http.StatusNotFound:
		return "No model with that name. Run `waired models ls` to see what this computer can run.", true
	case parsed.Code == "model_retired":
		// The daemon names the successor, and it is the only party that
		// knows it. Its sentence verbatim rather than a rewrite that
		// could name a different model than the one it resolved (#200).
		return parsed.Message, true
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

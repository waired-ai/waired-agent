package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/download"
)

func newModelsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Manage the models on this computer (ls / pull / use / cancel / rm / refresh / check-agent)",
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(newModelsLsCmd(), newModelsPullCmd(), newModelsUseCmd(), newModelsCancelCmd(),
		newModelsRmCmd(), newModelsRefreshCmd(), newModelsCheckAgentCmd())
	return cmd
}

// formatRefreshApplyHint names the command that actually applies a
// picker suggestion.
//
// This used to read "To apply, restart waired-agent (Step 12 swap will
// land via /waired/v1/models/refresh)" — a roadmap step number the
// reader has no way to resolve, an endpoint that was never built, and,
// since waired#812 made a model switch apply in process, a restart that
// is not needed either. `waired models use` (waired-agent#753) is the
// real answer, so name it. Pure so the wording is testable.
func formatRefreshApplyHint(modelID string) string {
	if modelID == "" {
		return `To apply, run "waired models use <model-id>".`
	}
	return fmt.Sprintf("To apply, run %q.", "waired models use "+modelID)
}

// newModelsRefreshCmd asks the management API what the auto-picker would
// choose now (= AvailableUpdate hint) and prints the answer. Reporting
// only: `waired models use` is what applies a choice.
func newModelsRefreshCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Show whether a better model pick is available for this computer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			body, err := httpGet(defaultMgmtURL + "/waired/v1/inference/status")
			if err != nil {
				return err
			}
			var st map[string]interface{}
			if err := json.Unmarshal(body, &st); err != nil {
				return err
			}
			avail, _ := st["available_update"].(map[string]interface{})
			if avail == nil {
				fmt.Fprintln(stdout, "No model update available. The current pick is already the best for this engine and computer.")
				return nil
			}
			fmt.Fprintf(stdout, "Update available: model=%v variant=%v precached=%v\n",
				avail["model_id"], avail["variant_id"], avail["precached"])
			modelID, _ := avail["model_id"].(string)
			fmt.Fprintln(stdout, formatRefreshApplyHint(modelID))
			return nil
		},
	}
	return cmd
}

func newModelsLsCmd() *cobra.Command {
	var mgmt, stateDir string
	var detail bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List the models on this computer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runModelsLsBody(mgmt, detail)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	cmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "directory holding identity.json (unused by ls)")
	cmd.Flags().BoolVar(&detail, "detail", false,
		"show recommended specs, hardware fit, and selection criteria from the model catalog")
	return cmd
}

func runModelsLsBody(mgmtVal string, detailVal bool) error {
	mgmt := &mgmtVal
	detail := &detailVal
	if *detail {
		// The catalog view (recommended specs + fit + criteria) reads the
		// richer /inference/catalog endpoint instead of the bare download
		// inventory below. See runModelsCatalog in models_catalog.go.
		return runModelsCatalog(*mgmt)
	}
	body, err := httpGet(*mgmt + "/waired/v1/models")
	if err != nil {
		return err
	}
	var resp struct {
		Models []struct {
			ModelID   string   `json:"model_id"`
			Aliases   []string `json:"aliases,omitempty"`
			State     string   `json:"state"`
			SizeBytes int64    `json:"size_bytes,omitempty"`
			VariantID string   `json:"variant_id,omitempty"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if len(resp.Models) == 0 {
		fmt.Fprintln(stdout, "(no models yet)")
		return nil
	}
	fmt.Fprintf(stdout, "%-30s %-12s %-8s %-12s %s\n", "MODEL_ID", "STATE", "SIZE", "VARIANT", "ALIASES")
	for _, m := range resp.Models {
		// HumanBytes, not a fixed GB divisor: the column only ever held
		// "-" before (#661), so this is the first time the unit matters,
		// and a 320 MB model reading "0.3GB" would be the same figure the
		// download bar shows as "320.0 MB" two commands earlier.
		size := "-"
		if m.SizeBytes > 0 {
			size = download.HumanBytes(m.SizeBytes)
		}
		fmt.Fprintf(stdout, "%-30s %-12s %-8s %-12s %s\n",
			m.ModelID, stateOrDash(m.State), size, defaultIfEmpty(m.VariantID, "-"),
			strings.Join(m.Aliases, ", "))
	}
	return nil
}

func newModelsPullCmd() *cobra.Command {
	var mgmt string
	var wait bool
	var assumeYes bool
	var force bool
	cmd := &cobra.Command{
		Use:   "pull <model_id|alias>",
		Short: "Download a model and wait until it's ready",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			model := args[0]
			// #61/#583: every fit warning is warn-then-honour — confirm
			// before pulling a model this host would not choose, or one
			// that does not fit its memory at all.
			proceed, err := confirmModelFitsForPull(mgmt, model, assumeYes, force, stdout, os.Stdin)
			if err != nil {
				return err
			}
			if !proceed {
				fmt.Fprintln(stdout, "pull cancelled.")
				return nil
			}
			body, _ := json.Marshal(map[string]string{"model": model})
			resp, err := httpPost(mgmt+"/waired/v1/models/pull", body)
			if err != nil {
				return err
			}
			var job struct {
				JobID   string `json:"job_id"`
				ModelID string `json:"model_id"`
				Status  string `json:"status"`
			}
			if err := json.Unmarshal(resp, &job); err != nil {
				return fmt.Errorf("decode: %w", err)
			}
			fmt.Fprintf(stdout, "queued pull: model=%s job=%s\n", model, job.JobID)

			if !wait {
				return nil
			}
			return waitForModelReady(mgmt, model, 30*time.Minute)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	cmd.Flags().BoolVar(&wait, "wait", true, "poll status until the model becomes ready")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the over-spec confirmation prompt")
	cmd.Flags().BoolVar(&force, "force", false,
		"with --yes, also confirm downloading a model that doesn't fit in this computer's memory")
	return cmd
}

func newModelsRmCmd() *cobra.Command {
	var mgmt, stateDir string
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "rm <model_id>",
		Short: "Remove a model",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// waired#845 §8.2: deleting weights gets an in-UI confirmation,
			// not an OS elevation prompt — the model can be downloaded
			// again, so the blast radius is bandwidth and time. Non-TTY
			// callers (scripts) must pass --yes explicitly.
			if !assumeYes && !confirmTTY(fmt.Sprintf("Remove model %q? You can download it again later", args[0])) {
				return errors.New("aborted, no changes made (pass --yes to skip the prompt)")
			}
			// Stop a download of this model first, and say so when there
			// was one: removing a model mid-pull used to answer "deleted"
			// while the job kept fetching, and the model came back
			// (waired-agent#641). The daemon cancels on its own behalf too
			// — the tray and the setup reconciler delete without coming
			// through here — so this is for the WORDING: an operator who
			// just stopped a 6 GB download should be told they did.
			if cancelled, err := httpDelete(mgmt + "/waired/v1/models/" + args[0] + "/pull"); err == nil {
				if line := formatModelsCancel(cancelled, args[0]); strings.HasPrefix(line, "cancelled ") {
					fmt.Fprintln(stdout, line)
				}
			}
			body, err := httpDelete(mgmt + "/waired/v1/models/" + args[0])
			if err != nil {
				return err
			}
			return prettyPrint(body)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	addStateDirFlag(cmd, &stateDir, "directory holding identity.json")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the removal confirmation prompt")
	return cmd
}

// waitForModelReady polls the inference status endpoint until the
// model lands in `ready`, `failed`, `not_present` after a download that
// was under way, or the deadline elapses. Prints a status line every
// poll so the CLI feels alive.
//
// The third ending is waired-agent#794. `models cancel` stops the job
// server-side and deletes its row, and a deleted row is reported as
// not_present — the same lane a model nobody has ever touched sits in.
// The wire cannot tell those apart on purpose (three histories, one
// answer), so the CLI keeps the one piece of history the wire does not
// have: whether THIS wait has seen the download running. Once it has, a
// return to not_present can only mean the download stopped.
//
// Before this, such a wait ran to its full 30-minute deadline and then
// reported a "timeout waiting for model to become ready" for something
// the operator had deliberately cancelled seconds earlier.
func waitForModelReady(mgmt, modelID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(modelWaitPoll)
	defer tick.Stop()
	lastReport := ""
	sawDownloading := false
	for {
		body, err := httpGet(mgmt + "/waired/v1/inference/status")
		if err != nil {
			fmt.Fprintln(stderr, "  (status unreachable:", err, ")")
		} else {
			line, done, err := parseModelLifecycle(body, modelID)
			switch modelLane(body, modelID) {
			case laneDownloading:
				sawDownloading = true
			case laneNotPresent:
				if sawDownloading {
					fmt.Fprintln(stdout, modelID+": download stopped before it finished")
					return errModelPullStopped
				}
			}
			if line != lastReport && line != "" {
				fmt.Fprintln(stdout, line)
				lastReport = line
			}
			if done {
				return err
			}
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for the model to become ready")
		}
		<-tick.C
	}
}

// errModelPullStopped is returned when a download this wait had watched
// running is no longer there. Non-zero exit for scripts, and the line
// above is the account a person reads — so main must not print a second
// one under it (exitPlanFor).
var errModelPullStopped = errors.New("")

// modelWaitPoll is how often waitForModelReady asks. A variable so the
// tests can shrink it, the way TestMain already shrinks hostSpeedAskPoll
// and engineWaitForStatus — a wait that spends the real interval per
// scripted snapshot costs more in CI than the behaviour it pins.
var modelWaitPoll = 3 * time.Second

// modelLanes mirrors the lifecycle lanes of
// /waired/v1/inference/status. The daemon projects seven model states
// onto these four (internal/management/inference_handlers.go), so
// not_present covers "never touched", "evicted", "deleted" AND "the
// pull was cancelled" — deliberately, since all four leave the host in
// the same state.
type modelLanes struct {
	Models struct {
		Ready       []string `json:"ready"`
		Downloading []string `json:"downloading"`
		Failed      []string `json:"failed"`
		Failures    []struct {
			Model string `json:"model"`
			Error string `json:"error"`
		} `json:"failures"`
		NotPresent []string `json:"not_present"`
	} `json:"models"`
}

func decodeModelLanes(body []byte) (modelLanes, bool) {
	var resp modelLanes
	if err := json.Unmarshal(body, &resp); err != nil {
		return modelLanes{}, false
	}
	return resp, true
}

// The lanes modelLane can report. Named rather than matched on the
// rendered line: the printed text is product copy and may be reworded,
// and a control-flow branch keyed on it would stop firing silently.
const (
	laneReady       = "ready"
	laneDownloading = "downloading"
	laneFailed      = "failed"
	laneNotPresent  = "not_present"
)

// modelLane reports which lifecycle lane the model sits in, or "" when
// the body does not decode or names the model in none of them.
func modelLane(body []byte, modelID string) string {
	resp, ok := decodeModelLanes(body)
	if !ok {
		return ""
	}
	for _, l := range []struct {
		name string
		ids  []string
	}{
		{laneReady, resp.Models.Ready},
		{laneDownloading, resp.Models.Downloading},
		{laneFailed, resp.Models.Failed},
		{laneNotPresent, resp.Models.NotPresent},
	} {
		if contains(l.ids, modelID) || aliasMatches(l.ids, modelID) {
			return l.name
		}
	}
	return ""
}

// parseModelLifecycle extracts a single-line status of the requested
// model from /waired/v1/inference/status. done==true means the
// caller should stop polling (ready or failed).
//
// A failure carries its reason on both the printed line and the returned
// error (waired-agent#328). Before that this said "failed" and "pull
// failed" and nothing else, on either — so an operator watching a
// download die had to go and read the daemon's journal to learn whether
// their disk was full or their engine had never started. The daemon knew
// the whole time; `failures` is that reason arriving.
func parseModelLifecycle(body []byte, modelID string) (line string, done bool, err error) {
	resp, ok := decodeModelLanes(body)
	if !ok {
		return "", false, nil
	}
	if contains(resp.Models.Ready, modelID) || aliasMatches(resp.Models.Ready, modelID) {
		return modelID + ": ready", true, nil
	}
	if contains(resp.Models.Downloading, modelID) || aliasMatches(resp.Models.Downloading, modelID) {
		return modelID + ": downloading...", false, nil
	}
	if contains(resp.Models.Failed, modelID) || aliasMatches(resp.Models.Failed, modelID) {
		// An older daemon, or a failure recorded before the reason was
		// stored, still reports the name alone. Degrade to what this
		// printed before rather than inventing a cause.
		for _, f := range resp.Models.Failures {
			if f.Model == modelID || strings.EqualFold(f.Model, modelID) {
				if f.Error != "" {
					return modelID + ": failed — " + f.Error, true,
						fmt.Errorf("pull failed: %s", f.Error)
				}
				break
			}
		}
		return modelID + ": failed", true, errors.New("pull failed")
	}
	// Nothing has started on it (waired-agent#403). Not terminal — the
	// pull this wait follows was just accepted, so the state row it writes
	// is moments away — but it has to SAY so: the daemon could not express
	// this state before, and the caller printed nothing at all until the
	// timeout, on a command whose whole job is to report progress.
	if contains(resp.Models.NotPresent, modelID) || aliasMatches(resp.Models.NotPresent, modelID) {
		return modelID + ": not started yet", false, nil
	}
	return "", false, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// aliasMatches treats `s` items as model_ids and returns true if any
// of them shares the same trailing segment as v (handles the case
// where the user passes an alias and the status uses the model_id).
func aliasMatches(s []string, v string) bool {
	for _, x := range s {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

func stateOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func defaultIfEmpty(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newModelsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "models",
		Short: "Manage local LLM models (ls / pull / rm / refresh).",
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(newModelsLsCmd(), newModelsPullCmd(), newModelsRmCmd(), newModelsRefreshCmd())
	return cmd
}

// newModelsRefreshCmd asks the management API what the auto-picker would
// choose now (= AvailableUpdate hint) and prints the answer. With
// --to X, it just shows the current picker decision against that
// engine constraint. The actual swap happens in Step 12.
func newModelsRefreshCmd() *cobra.Command {
	var to string
	var yes bool
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Show whether a better model pick is available for this host.",
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
				fmt.Println("No model update available; current pick is already optimal for this engine + host.")
				return nil
			}
			fmt.Printf("Update available: model=%v variant=%v precached=%v\n",
				avail["model_id"], avail["variant_id"], avail["precached"])
			if to != "" {
				fmt.Printf("(--to %s recorded; honoured by the swap implementation in Step 12)\n", to)
			}
			_ = yes
			fmt.Println("To apply, restart waired-agent (Step 12 swap will land via /waired/v1/models/refresh).")
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "force model_id (currently informational only)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip interactive confirmation")
	return cmd
}

func newModelsLsCmd() *cobra.Command {
	var mgmt, stateDir string
	var detail bool
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List registered models.",
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
		fmt.Println("(no models registered)")
		return nil
	}
	fmt.Printf("%-30s %-12s %-8s %-12s %s\n", "MODEL_ID", "STATE", "SIZE", "VARIANT", "ALIASES")
	for _, m := range resp.Models {
		size := "-"
		if m.SizeBytes > 0 {
			size = fmt.Sprintf("%.1fGB", float64(m.SizeBytes)/1e9)
		}
		fmt.Printf("%-30s %-12s %-8s %-12s %s\n",
			m.ModelID, stateOrDash(m.State), size, defaultIfEmpty(m.VariantID, "-"),
			strings.Join(m.Aliases, ", "))
	}
	return nil
}

func newModelsPullCmd() *cobra.Command {
	var mgmt string
	var wait bool
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "pull <model_id|alias>",
		Short: "Pull a model and (by default) wait until it is ready.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			model := args[0]
			// #61: over-spec picks are warn-but-allow — confirm before
			// pulling a model that exceeds this host's recommended spec.
			proceed, err := confirmModelFitsForPull(mgmt, model, assumeYes, os.Stdout, os.Stdin)
			if err != nil {
				return err
			}
			if !proceed {
				fmt.Println("pull cancelled.")
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
			fmt.Printf("queued pull: model=%s job=%s\n", model, job.JobID)

			if !wait {
				return nil
			}
			return waitForModelReady(mgmt, model, 30*time.Minute)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	cmd.Flags().BoolVar(&wait, "wait", true, "poll status until the model becomes ready")
	cmd.Flags().BoolVarP(&assumeYes, "yes", "y", false, "skip the over-spec confirmation prompt")
	return cmd
}

func newModelsRmCmd() *cobra.Command {
	var mgmt, stateDir string
	var assumeYes bool
	cmd := &cobra.Command{
		Use:   "rm <model_id>",
		Short: "Remove a model.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// waired#845 §8.2: deleting weights gets an in-UI confirmation,
			// not an OS elevation prompt — the model can be downloaded
			// again, so the blast radius is bandwidth and time. Non-TTY
			// callers (scripts) must pass --yes explicitly.
			if !assumeYes && !confirmTTY(fmt.Sprintf("Remove model %q? You can download it again later", args[0])) {
				return errors.New("models rm: aborted (pass --yes to skip the prompt)")
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
// model lands in `ready`, `failed`, or the deadline elapses. Prints
// a status line every poll so the CLI feels alive.
func waitForModelReady(mgmt, modelID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	lastReport := ""
	for {
		body, err := httpGet(mgmt + "/waired/v1/inference/status")
		if err != nil {
			fmt.Fprintln(os.Stderr, "  (status unreachable:", err, ")")
		} else {
			line, done, err := parseModelLifecycle(body, modelID)
			if line != lastReport && line != "" {
				fmt.Println(line)
				lastReport = line
			}
			if done {
				return err
			}
		}
		if time.Now().After(deadline) {
			return errors.New("timeout waiting for model to become ready")
		}
		<-tick.C
	}
}

// parseModelLifecycle extracts a single-line status of the requested
// model from /waired/v1/inference/status. done==true means the
// caller should stop polling (ready or failed).
func parseModelLifecycle(body []byte, modelID string) (line string, done bool, err error) {
	var resp struct {
		Models struct {
			Ready       []string `json:"ready"`
			Downloading []string `json:"downloading"`
			Failed      []string `json:"failed"`
		} `json:"models"`
	}
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return "", false, nil
	}
	if contains(resp.Models.Ready, modelID) || aliasMatches(resp.Models.Ready, modelID) {
		return modelID + ": ready", true, nil
	}
	if contains(resp.Models.Downloading, modelID) || aliasMatches(resp.Models.Downloading, modelID) {
		return modelID + ": downloading…", false, nil
	}
	if contains(resp.Models.Failed, modelID) || aliasMatches(resp.Models.Failed, modelID) {
		return modelID + ": failed", true, errors.New("pull failed")
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

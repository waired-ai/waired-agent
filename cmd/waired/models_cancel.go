package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
)

// newModelsCancelCmd stops a download that is running.
//
// It exists because `models rm` was the only way out of a pull started by
// mistake (waired-agent#633), and it did not actually stop one: it
// deleted the catalog row, which is the only thing that showed the
// download, while the job kept fetching and wrote the model back as ready
// (waired-agent#641).
//
// No confirmation prompt. Every other destructive verb here has one
// because it throws away something the host has; this throws away
// something the host is in the middle of acquiring, which is what the
// operator just said they did not want.
func newModelsCancelCmd() *cobra.Command {
	var mgmt string
	cmd := &cobra.Command{
		Use:   "cancel <model_id>",
		Short: "Stop a download that is running",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			body, err := httpDelete(mgmt + "/waired/v1/models/" + args[0] + "/pull")
			if err != nil {
				return err
			}
			fmt.Fprintln(stdout, formatModelsCancel(body, args[0]))
			return nil
		},
	}
	addMgmtFlag(cmd, &mgmt)
	return cmd
}

// formatModelsCancel renders the daemon's answer as the one line the
// operator needs. Pure, so the wording is testable without a daemon.
//
// A body this build does not recognise is reported as the cancel having
// been made rather than as an error: the request returned 200, so the
// daemon acted, and inventing a failure from an unread field would be a
// worse answer than a plain one.
func formatModelsCancel(body []byte, requested string) string {
	var res struct {
		ModelID string `json:"model_id"`
		JobID   string `json:"job_id"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "cancelled download: model=" + requested
	}
	model := res.ModelID
	if model == "" {
		model = requested
	}
	// "nothing to stop" is not a failure — the host is in the state the
	// operator asked for. Exit 0, same as `models ls` on an empty catalog
	// and `models pull` on a declined confirmation.
	if res.Status == "not_downloading" {
		return "no download in progress for " + model
	}
	if res.JobID == "" {
		return "cancelled download: model=" + model
	}
	return "cancelled download: model=" + model + " job=" + res.JobID
}

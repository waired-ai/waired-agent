package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/agentgrade"
)

// newModelsCheckAgentCmd adds `waired models check-agent`.
//
// It answers the question a user has BEFORE spending an hour and tens
// of gigabytes on a model: will this thing actually work with my coding
// agent? That is not the same question as "does it fit" (the model
// catalog answers that) or "is it fast enough" (the benchmark answers
// that). A model can fit, decode quickly, advertise tool support, and
// still hand the agent a JSON object as prose — which is the defect
// this checks for, and the one that is invisible until you hit it.
//
// The check is the same probe the maintainers run when deciding whether
// a model belongs in the catalog at all. One implementation, two
// callers: this command, and the manual CI lane.
func newModelsCheckAgentCmd() *cobra.Command {
	var stateDir, gateway, jsonOut string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "check-agent [model]",
		Short: "Check whether a model can drive a coding agent's tool calls.",
		Long: `Check whether a model can drive a coding agent's tool calls.

Sends this device's inference gateway a request shaped like the ones a
coding agent sends — a large instruction prompt plus a full set of tool
definitions — and reports whether the model answers with proper tool
calls or with something the agent cannot use.

Models that fail this usually look fine everywhere else: they load, they
answer questions, and they are fast. The failure only shows up once a
coding agent is driving them, and it shows up as the agent printing raw
JSON at you, or calling tools that do not exist.

With no argument, checks the model this device currently serves.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			model := ""
			if len(args) == 1 {
				model = args[0]
			}
			return runModelsCheckAgent(checkAgentOpts{
				Model:    model,
				StateDir: stateDir,
				Gateway:  gateway,
				JSONOut:  jsonOut,
				Timeout:  timeout,
			})
		},
	}
	claudeStateDirFlag(cmd, &stateDir)
	cmd.Flags().StringVar(&gateway, "gateway", "",
		"Anthropic-compatible gateway base URL (default: this device's, from agent.json)")
	cmd.Flags().StringVar(&jsonOut, "json", "", "also write the full result to this file as JSON")
	cmd.Flags().DurationVar(&timeout, "timeout", agentgrade.DefaultTimeout,
		"per-question time limit")
	return cmd
}

type checkAgentOpts struct {
	Model    string
	StateDir string
	Gateway  string
	JSONOut  string
	Timeout  time.Duration
}

func runModelsCheckAgent(o checkAgentOpts) error {
	base := o.Gateway
	if base == "" {
		base, _ = claudeBaseURL(o.StateDir)
	}
	model := o.Model
	if model == "" {
		// The dynamic coding alias resolves to whatever this device
		// actually serves, which is the model the user's coding agent
		// would be talking to. Naming it explicitly would require
		// reading the active model out of the daemon and would then
		// disagree with the gateway whenever a swap was in flight.
		model = "waired/default"
	}

	fmt.Fprintf(stdout, "Checking %s …\n", displayModel(o.Model))
	fmt.Fprintf(stdout, "This sends a few real requests through this device, so it takes a minute.\n\n")

	probe := agentgrade.Probe{BaseURL: base, Timeout: o.Timeout}
	rep, err := probe.Run(context.Background(), model)
	if err != nil {
		return fmt.Errorf("check-agent: %w", err)
	}
	rep.Model = displayModel(o.Model)

	printCheckAgentReport(rep)

	if o.JSONOut != "" {
		if err := writeCheckAgentJSON(o.JSONOut, rep); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "\nFull result written to %s\n", o.JSONOut)
	}

	switch rep.Grade {
	case agentgrade.GradePass:
		return nil
	case agentgrade.GradeUnknown:
		// Not a verdict about the model. Saying "failed" here would
		// blame the model for the engine being down, which is the
		// mistake #203 records in the benchmark path.
		return fmt.Errorf("could not complete the check: %s", rep.Error)
	default:
		return fmt.Errorf("this model is not reliable with coding agents (see above)")
	}
}

func displayModel(requested string) string {
	if requested == "" {
		return "the model this device is serving"
	}
	return requested
}

// printCheckAgentReport writes the result for a person, not a log
// parser. It says what happened and what to do about it; the reason
// codes and evidence go to the --json file.
func printCheckAgentReport(rep agentgrade.Report) {
	for _, r := range rep.Results {
		fmt.Fprintf(stdout, "  %s  %s\n", statusMark(r.Verdict), checkAgentCaseLine(r))
	}
	fmt.Fprintln(stdout)

	switch rep.Grade {
	case agentgrade.GradePass:
		fmt.Fprintf(stdout, "OK — %s works with coding agents.\n", rep.Model)
	case agentgrade.GradeUnknown:
		fmt.Fprintf(stdout, "Could not check %s.\n\n", rep.Model)
		fmt.Fprintf(stdout, "  %s\n\n", rep.Error)
		fmt.Fprintf(stdout, "This says nothing about the model — the check could not get an answer at all.\n")
		fmt.Fprintf(stdout, "Make sure the model is downloaded and this device is running, then try again:\n")
		fmt.Fprintf(stdout, "  waired status\n")
		fmt.Fprintf(stdout, "  waired models ls\n")
	default:
		fmt.Fprintf(stdout, "Not recommended — %s is unreliable with coding agents.\n\n", rep.Model)
		fmt.Fprintf(stdout, "It will usually look like the agent printing raw text at you instead of\n")
		fmt.Fprintf(stdout, "doing the work, or trying to use tools that do not exist. Nothing is\n")
		fmt.Fprintf(stdout, "broken on this device; the model just cannot follow the format.\n\n")
		fmt.Fprintf(stdout, "Pick a different model:\n")
		fmt.Fprintf(stdout, "  waired models ls --detail\n")
	}
}

func statusMark(v agentgrade.Verdict) string {
	switch {
	case v == agentgrade.VerdictPass:
		return "ok  "
	case v == agentgrade.VerdictError:
		return "??  "
	case v.IsFailure(): //nolint:staticcheck // not a tagged switch: IsFailure covers several values
		return "FAIL"
	default:
		return "warn"
	}
}

// checkAgentCaseLine describes one question in plain language. The
// internal case names and verdict codes are for the JSON file; a user
// reading a terminal needs to know what was asked and what came back.
func checkAgentCaseLine(r agentgrade.Result) string {
	what := map[string]string{
		"greeting":         "Answers a plain greeting without inventing a tool call",
		"read-file":        "Uses a tool when it needs to read a file",
		"search-then-edit": "Picks the right tool for a search",
	}[r.Case]
	if what == "" {
		what = r.Case
	}
	switch r.Verdict {
	case agentgrade.VerdictPass:
		return what
	case agentgrade.VerdictError:
		return what + " — could not check: " + r.Detail
	default:
		return what + " — " + r.Detail
	}
}

func writeCheckAgentJSON(path string, rep agentgrade.Report) error {
	// rep already carries the fixture revision (Probe.Run sets it), so a
	// result recorded today can be told apart from one measured against
	// a different request weight later.
	b, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		return fmt.Errorf("check-agent: encode result: %w", err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("check-agent: write %s: %w", path, err)
	}
	return nil
}

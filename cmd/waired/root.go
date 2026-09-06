package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/buildinfo"
)

// namespaceRunE is the RunE for a pure-namespace command (one with only
// subcommands). It prints help and fails: naming a namespace is not a
// request this program can carry out, whichever way it was reached.
// Setting RunE makes the command "runnable" so cobra validates args at
// every nesting level — its built-in unknown-subcommand error only fires
// at the root, so nested namespaces (e.g. `inference engine`) need this.
//
// No args used to exit 0 after printing help, which made `waired peers`
// indistinguishable from a listing that found nothing: a script got
// success and no data (#661). Both branches are the same mistake — a
// namespace was named where a verb belongs — so both now exit non-zero,
// and help is printed either way because the fix is the same: pick a
// subcommand from the list.
//
// Exit 1, the code every other CLI error uses (exitPlanFor). A distinct
// code would be a contract the installers and docs would have to carry,
// and there is nothing here a caller would branch on differently.
func namespaceRunE(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		// Help on stdout — it is the answer to "what can I run" — then a
		// bare error, since cmd.Help() has already said everything a
		// usage dump would repeat.
		if err := cmd.Help(); err != nil {
			return err
		}
		cmd.SilenceUsage = true
		return fmt.Errorf("%q needs a subcommand", cmd.CommandPath())
	}
	return fmt.Errorf("unknown subcommand %q for %q", args[0], cmd.CommandPath())
}

// defaultMgmtURL / defaultGatewayURL are the loopback defaults shared by
// the subcommands that talk to the local daemon. They mirror the values
// the old hand-rolled flag sets used so the CLI surface is unchanged.
// There is one local gateway. `waired infer` used to point at a second,
// token-less listener on :9479 because :9473 required a Bearer read from a
// root-owned 0600 file that a non-root one-shot CLI could not open (#598).
// The token is gone and so is the second listener (waired-ai/waired#1277),
// so both spellings collapse into one.
//
// These stay compiled-in constants because cobra fixes flag defaults before
// --state-dir is parsed; resolveGatewayBaseURL reads the host's real port
// from agent.json at run time when the flag was not given.
const (
	defaultMgmtURL    = "http://127.0.0.1:9476"
	defaultGatewayURL = "http://127.0.0.1:9473"
)

// rootLong is the top-level description shown by `waired --help`. The
// per-command detail lives in each command's Long; this stays short.
const rootLong = `waired drives the local waired-agent daemon (status / ping / inference over
the Local Management API on 127.0.0.1:9476) and the Control Plane during
enrollment (waired init).

Run 'waired <command> --help' for command-specific flags.`

// commandGroups defines the grouped sections the top-level help renders,
// mirroring the grouping the old printUsage() conveyed.
func commandGroups() []*cobra.Group {
	return []*cobra.Group{
		{ID: "setup", Title: "Setup & identity:"},
		{ID: "agents", Title: "Coding agents:"},
		{ID: "inference", Title: "Models & inference:"},
		{ID: "routing", Title: "Routing control:"},
		{ID: "maint", Title: "Maintenance:"},
	}
}

// groupFor maps a command name to its top-level group ID.
var groupFor = map[string]string{
	"init": "setup", "status": "setup", "doctor": "setup", "auth": "setup", "logout": "setup",
	"link": "agents", "unlink": "agents", "claude": "agents",
	"models": "inference", "runtimes": "inference", "infer": "inference",
	"inference": "inference", "public": "inference", "worker": "inference", "peers": "inference", "ping": "inference",
	"share": "inference",
	"pause": "routing", "resume": "routing",
	"version": "maint", "update": "maint", "keygen": "maint", "config": "maint", "logs": "maint",
}

// newRetiredProxyCmd preserves a helpful redirect for the retired `waired
// proxy` command — the MITM transparent proxy was replaced by the managed
// -settings ANTHROPIC_BASE_URL integration in #488. Hidden so it does not
// appear in help, but typing it (or any old `proxy <verb>`) points the
// operator at the replacement rather than failing with a bare "unknown
// command". DisableFlagParsing so old flag forms route here too.
func newRetiredProxyCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "proxy",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("`waired proxy` was removed in favour of managed settings; use `waired claude <enable|disable|status>`")
		},
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "waired",
		Short: "Waired CLI — drive the local waired-agent daemon and Control Plane enrollment",
		Long:  rootLong,
		// RunE errors are printed once as "waired: <err>" by main(); cobra
		// must not also print the error or dump usage on a runtime failure.
		SilenceUsage:  true,
		SilenceErrors: true,
		// `waired` with no args prints help and exits 0 (conventional).
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
		Version: buildinfo.Short(),
	}
	root.SetVersionTemplate("waired {{.Version}}\n")
	// cobra writes help, usage and the version through its own writers, which
	// default to the process's streams. Point them at the folding pair so a
	// `Long:` with an em dash degrades like everything else (waired-agent#1105);
	// cmd.OutOrStdout() inside a RunE then inherits it too.
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.AddGroup(commandGroups()...)

	cmds := []*cobra.Command{
		newInitCmd(),
		newStatusCmd(),
		newDoctorCmd(),
		newAuthCmd(),
		newLogoutCmd(),
		newLinkCmd(),
		newUnlinkCmd(),
		newClaudeCmd(),
		newRetiredProxyCmd(),
		newModelsCmd(),
		newRuntimesCmd(),
		newInferCmd(),
		newInferenceCmd(),
		newShareCmd(),
		newPublicCmd(),
		newWorkerCmd(),
		newPeersCmd(),
		newPingCmd(),
		newPauseCmd(),
		newResumeCmd(),
		newVersionCmd(),
		newUpdateCmd(),
		newKeygenCmd(),
		newConfigCmd(),
		newLogsCmd(),
	}
	for _, c := range cmds {
		c.GroupID = groupFor[c.Name()]
		root.AddCommand(c)
	}

	// Place cobra's built-in help / completion commands in the Maintenance
	// group so they don't render as an ungrouped trailing section.
	root.SetHelpCommandGroupID("maint")
	root.SetCompletionCommandGroupID("maint")
	return root
}

// addMgmtFlag / addStateDirFlag / addGatewayFlag attach the shared
// daemon-facing flags to the commands that accept them. They are NOT
// registered as persistent root flags on purpose: commands like `version`
// and `keygen` must keep rejecting `--mgmt`, exactly as before.
func addMgmtFlag(cmd *cobra.Command, p *string) {
	cmd.Flags().StringVar(p, "mgmt", defaultMgmtURL, "Local Management API base URL")
}

func addStateDirFlag(cmd *cobra.Command, p *string, usage string) {
	cmd.Flags().StringVar(p, "state-dir", defaultStateDir(), usage)
}

func addGatewayFlag(cmd *cobra.Command, p *string) {
	cmd.Flags().StringVar(p, "gateway", defaultGatewayURL,
		"Local Gateway base URL for 'waired infer' (default is the token-less loopback gateway)")
}

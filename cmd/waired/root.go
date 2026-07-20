package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/buildinfo"
)

// namespaceRunE is the RunE for a pure-namespace command (one with only
// subcommands). No args prints help and exits 0; an unrecognized subcommand
// is an error. Setting RunE makes the command "runnable" so cobra validates
// args at every nesting level — its built-in unknown-subcommand error only
// fires at the root, so nested namespaces (e.g. `inference share`) need this.
func namespaceRunE(cmd *cobra.Command, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	return fmt.Errorf("unknown subcommand %q for %q", args[0], cmd.CommandPath())
}

// defaultMgmtURL / defaultGatewayURL are the loopback defaults shared by
// the subcommands that talk to the local daemon. They mirror the values
// the old hand-rolled flag sets used so the CLI surface is unchanged.
// defaultInferGatewayURL points `waired infer` at the token-less data-plane
// gateway instead: :9473 requires the Bearer from <state>/secrets/gateway-token
// (root-owned 0600 on service installs), which a non-root one-shot CLI cannot
// read — :9479 trusts loopback processes by design, same as the coding-agent
// integrations (#598).
const (
	defaultMgmtURL         = "http://127.0.0.1:9476"
	defaultGatewayURL      = "http://127.0.0.1:9473"
	defaultInferGatewayURL = "http://127.0.0.1:9479"
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
	"link": "agents", "unlink": "agents", "claude": "agents", "codeui": "agents",
	"models": "inference", "runtimes": "inference", "infer": "inference",
	"inference": "inference", "public": "inference", "worker": "inference", "peers": "inference", "ping": "inference",
	"pause": "routing", "resume": "routing",
	"version": "maint", "update": "maint", "keygen": "maint",
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
		Short: "Waired CLI — drive the local waired-agent daemon and Control Plane enrollment.",
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
		newCodeuiCmd(),
		newRetiredProxyCmd(),
		newModelsCmd(),
		newRuntimesCmd(),
		newInferCmd(),
		newInferenceCmd(),
		newPublicCmd(),
		newWorkerCmd(),
		newPeersCmd(),
		newPingCmd(),
		newPauseCmd(),
		newResumeCmd(),
		newVersionCmd(),
		newUpdateCmd(),
		newKeygenCmd(),
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
	cmd.Flags().StringVar(p, "gateway", defaultInferGatewayURL,
		"Local Gateway base URL for 'waired infer' (default is the token-less loopback gateway)")
}

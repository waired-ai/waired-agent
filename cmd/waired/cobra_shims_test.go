package main

import "github.com/spf13/cobra"

// execCobra builds and runs a cobra command with args, returning the RunE
// error. SilenceErrors/Usage keep the returned error clean (no
// double-printing) so tests can assert on it directly. This mirrors what
// the production CLI does for each command via newRootCmd().Execute().
func execCobra(cmd *cobra.Command, args []string) error {
	cmd.SetArgs(args)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	return cmd.Execute()
}

// Thin test shims preserving the pre-cobra dispatcher signatures the
// existing tests call. Each builds the corresponding cobra command and
// executes it, exercising the real flag-parsing + subcommand-dispatch path.
func runWorker(args []string) error    { return execCobra(newWorkerCmd(), args) }
func runPeers(args []string) error     { return execCobra(newPeersCmd(), args) }
func runInference(args []string) error { return execCobra(newInferenceCmd(), args) }
func runPublic(args []string) error    { return execCobra(newPublicCmd(), args) }
func runLogout(args []string) error    { return execCobra(newLogoutCmd(), args) }
func runPause(args []string) error     { return execCobra(newPauseCmd(), args) }
func runResume(args []string) error    { return execCobra(newResumeCmd(), args) }

func runLink(args []string, uninstall bool) error {
	if uninstall {
		return execCobra(newUnlinkCmd(), args)
	}
	return execCobra(newLinkCmd(), args)
}

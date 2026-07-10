package main

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/buildinfo"
)

// newVersionCmd implements `waired version`. The human form is the default;
// `--json` emits {version, buildSHA, os, arch} — the stable interface the
// install scripts read to learn the currently-installed version (#292),
// and that `waired update` (#293) builds on.
func newVersionCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the waired build version (--json for {version, buildSHA, os, arch}).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return writeVersion(cmd.OutOrStdout(), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit {version, buildSHA, os, arch} as JSON")
	return cmd
}

// writeVersion renders the version to w; split out from runVersion so it
// can be exercised without capturing os.Stdout.
func writeVersion(w io.Writer, asJSON bool) error {
	if asJSON {
		return json.NewEncoder(w).Encode(map[string]string{
			"version":  buildinfo.Version,
			"buildSHA": buildinfo.BuildSHA,
			"os":       runtime.GOOS,
			"arch":     runtime.GOARCH,
		})
	}
	_, err := fmt.Fprintf(w, "waired %s\n", buildinfo.Short())
	return err
}

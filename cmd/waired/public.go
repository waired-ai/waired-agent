package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/internal/management/observabilityclient"
	"github.com/waired-ai/waired-agent/internal/observability"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// publicLong describes the `waired public` command group. Public Share
// lets this computer take work from — and hand work to — machines shared
// by other Waired users, subject to a first-use security warning. All
// wording here is plain English: no Waired-internal vocabulary.
const publicLong = `Control Public Share — using and sharing machines with other Waired users:

  waired public status   Show whether this computer is shared publicly and
      whether this computer is allowed to use other people's public machines.

Public machines are other people's computers. A security and privacy
warning is shown before you can start using them.`

func newPublicCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "public",
		Short: "Use and share machines publicly with other Waired users.",
		Long:  publicLong,
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(newPublicStatusCmd())
	return cmd
}

func newPublicStatusCmd() *cobra.Command {
	var mgmt string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show public sharing and public-use settings for this computer.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPublicStatus(mgmt, jsonOut, os.Stdout)
		},
	}
	addMgmtFlag(cmd, &mgmt)
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the raw status objects as JSON")
	return cmd
}

// runPublicStatus renders both halves of the Public Share picture: the
// provider side (is this computer shared publicly) from GET
// /waired/v1/public/share, and the consumer side (may this computer use
// other people's public machines) from GET /waired/v1/public/use. Each
// half degrades independently: a 404 on one route (an older daemon that
// exposes only the other family) must not abort the other block.
func runPublicStatus(mgmt string, jsonOut bool, out io.Writer) error {
	var share *management.PublicShareStateResponse
	shareSupported := true
	{
		var resp management.PublicShareStateResponse
		if err := publicGetJSON(mgmt, "/waired/v1/public/share", &resp); err != nil {
			if isMgmtStatus(err, http.StatusNotFound) {
				shareSupported = false
			} else {
				return fmt.Errorf("waired public status: %w", err)
			}
		} else {
			share = &resp
		}
	}

	var use *management.PublicUseResponse
	useSupported := true
	{
		var resp management.PublicUseResponse
		if err := publicGetJSON(mgmt, "/waired/v1/public/use", &resp); err != nil {
			if isMgmtStatus(err, http.StatusNotFound) {
				useSupported = false
			} else {
				return fmt.Errorf("waired public status: %w", err)
			}
		} else {
			use = &resp
		}
	}

	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Share *management.PublicShareStateResponse `json:"share"`
			Use   *management.PublicUseResponse        `json:"use"`
		}{Share: share, Use: use})
	}

	// Provider side — is this computer shared publicly?
	if !shareSupported {
		pln(out, "Sharing this computer: unsupported by this daemon (upgrade waired-agent)")
	} else {
		pf(out, "Sharing this computer: %s\n", renderPublicShareState(share.State))
		if share.CPSynced != nil && !*share.CPSynced && share.Note != "" {
			// Verbatim, server-sourced plain-English explanation of the
			// not-yet-synced state (management.PublicSharePendingNote).
			pln(out, share.Note)
		}
		// GET /public/share does not populate MaxClients — only the
		// enable/disable responses carry the control-plane-echoed cap.
		pln(out, "Guest limit: not reported by this daemon")
	}

	// Consumer side — may this computer use other people's public machines?
	if !useSupported {
		pln(out, "Use public nodes: unsupported by this daemon (upgrade waired-agent)")
	} else {
		mode := use.EffectiveMode
		if mode == "" {
			mode = use.Mode
		}
		pf(out, "Use public nodes: %s\n", mode)
		pf(out, "Consented: %s\n", publicYesNo(use.Consented))
		pf(out, "Minimum quality tier: %d\n", use.MinQualityTier)
		pf(out, "Main agent: %s\n", publicOnOff(use.Main))
		pf(out, "Sub agents: %s\n", publicOnOff(use.Sub))

		// The nudge only makes sense before consent — once consented, the
		// consumer block already reflects the enabled state.
		if !use.Consented {
			if msg := publicNudgeMessage(mgmt); msg != "" {
				pln(out, msg)
			}
		}
	}

	return nil
}

// renderPublicShareState maps the wire state string to plain English.
// state.PublicShareOn/Off are the only recognised values; an empty state
// means this device has no Public Share controller wired (not enrolled),
// and anything else is a daemon newer than this CLI.
func renderPublicShareState(v string) string {
	switch v {
	case string(state.PublicShareOn):
		return "on"
	case string(state.PublicShareOff):
		return "off"
	case "":
		return "unknown (this device is not enrolled)"
	default:
		return fmt.Sprintf("%s (unrecognised — check daemon version)", v)
	}
}

// publicNudgeMessage returns the server-sourced pre-consent Public Share
// hint verbatim, or "" when there is none (or the daemon predates the
// observability endpoints). The message text is DATA supplied by the
// daemon — never hardcoded here — so a single source drives every UI
// surface. The event's Reason is a filtering tag and is never rendered.
func publicNudgeMessage(mgmt string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := observabilityclient.GetEvents(ctx, mgmt, 0,
		[]observability.Kind{observability.KindPublicShareNudge}, 1)
	if err != nil {
		// ErrUnsupported (older daemon) or any transport/parse error:
		// the nudge is a best-effort hint, so stay silent.
		return ""
	}
	for _, ev := range resp.Events {
		if ev.PublicShareNudge != nil {
			return ev.PublicShareNudge.Message
		}
	}
	return ""
}

func publicOnOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func publicYesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// pln / pf write a status line to out, ignoring the write error — status
// output goes to stdout or a test buffer, where a write failure is not
// actionable (errcheck would otherwise flag every Fprint call).
func pln(w io.Writer, s string)               { _, _ = io.WriteString(w, s+"\n") }
func pf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

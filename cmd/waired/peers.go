package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
)

// runPeers dispatches `waired peers <subcommand>`. Only `list` for
// now — the subcommand layer exists so a future `waired peers
// rename` / `peers describe` slots in without breaking the command tree.
const peersLong = `List known mesh peers (DeviceID, IP, engine, GPU, model) so the operator
can pick a '--pin' target for 'worker set'.

  waired peers list [--json]`

func newPeersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "peers",
		Short: "List known mesh peers (for picking a 'worker set --pin' target).",
		Long:  peersLong,
		RunE:  namespaceRunE,
	}
	cmd.AddCommand(newPeersListCmd())
	return cmd
}

func newPeersListCmd() *cobra.Command {
	var mgmt string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Render the mesh-peer snapshot (name, DeviceID, IP, engine, GPU, models, worker-capable).",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			snap, err := fetchMeshSnapshot(mgmt, 2*time.Second)
			if err != nil {
				return fmt.Errorf("waired peers list: %w", err)
			}
			if jsonOut {
				return writePeersJSON(os.Stdout, snap)
			}
			writePeersTable(os.Stdout, snap)
			return nil
		},
	}
	cmd.Flags().StringVar(&mgmt, "mgmt", defaultMgmtAddr, "Local Management API base URL")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit JSON instead of a tab-aligned table")
	return cmd
}

// writePeersTable mirrors writeMeshTable (cmd/waired/claude.go) but
// drops the columns unrelated to peer selection and adds a final
// WORKER-CAPABLE column derived from the same filter the daemon
// applies when deciding whether a peer can serve inference. The self
// row is excluded — `waired worker set --pin=<self>` makes no sense.
func writePeersTable(w io.Writer, m *inferencemesh.Snapshot) {
	if m == nil || len(m.Peers) == 0 {
		_, _ = fmt.Fprintln(w, "no peers in current mesh snapshot")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tDEVICE-ID\tOVERLAY-IP\tENGINE\tGPU\tVRAM\tMODELS\tWORKER-CAPABLE")
	for _, p := range m.Peers {
		_, _ = fmt.Fprintln(tw, peerRow(p))
	}
	_ = tw.Flush()
}

func peerRow(p inferencemesh.PeerView) string {
	name := p.DeviceName
	if name == "" {
		name = "-"
	}
	overlay := p.OverlayIP
	if overlay == "" {
		overlay = "-"
	}
	engine := "-"
	gpu := "-"
	vram := "-"
	models := "-"
	if p.InferenceState != nil {
		if t := p.InferenceState.Type; t != "" {
			engine = t
		}
		if hw := p.InferenceState.Hardware; hw != nil && len(hw.GPUs) > 0 {
			gpu = hw.GPUs[0].Model
			if mb := hw.GPUs[0].VRAMTotalMB; mb > 0 {
				vram = fmt.Sprintf("%d GB", (mb+512)/1024)
			}
		}
		if len(p.InferenceState.Models) > 0 {
			models = strings.Join(p.InferenceState.Models, ",")
		}
	}
	var capable string
	switch {
	case p.InferenceState == nil:
		capable = "no (no engine)"
	case p.Stale:
		capable = "stale"
	case !p.InferenceState.Reachable:
		capable = "unreachable"
	case len(p.InferenceState.Models) == 0:
		capable = "no (no model)"
	default:
		capable = "yes"
	}
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
		name, peerDisplayID(p), overlay, engine, gpu, vram, models, capable)
}

// peerDisplayID is the identifier this listing may show for a peer.
//
// A Public Share peer is a stranger's machine injected under a grant;
// only the grant pseudonym for its owner account may be displayed, never
// the real device identifier (public share spec §8.5). Own-network peers
// carry no grant and are shown by DeviceID as before.
func peerDisplayID(p inferencemesh.PeerView) string {
	if p.Grant != nil && p.Grant.Pseudonym != "" {
		return p.Grant.Pseudonym
	}
	return p.DeviceID
}

func writePeersJSON(w io.Writer, m *inferencemesh.Snapshot) error {
	type envelope struct {
		Peers []inferencemesh.PeerView `json:"peers"`
	}
	if m == nil {
		return json.NewEncoder(w).Encode(envelope{Peers: nil})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(envelope{Peers: scrubPeersForDisplay(m.Peers)})
}

// scrubPeersForDisplay replaces a Public Share peer's real device
// identifier with its grant pseudonym before the snapshot is printed
// (spec §8.5). Returns a copy: the caller's snapshot is shared with the
// table renderer and must not be mutated.
//
// DeviceName is left alone — the control plane already substitutes the
// pseudonym there at injection time — and the grant itself stays, since
// it carries only the pseudonym, the kind and the role.
func scrubPeersForDisplay(peers []inferencemesh.PeerView) []inferencemesh.PeerView {
	if len(peers) == 0 {
		return peers
	}
	out := make([]inferencemesh.PeerView, len(peers))
	copy(out, peers)
	for i := range out {
		out[i].DeviceID = peerDisplayID(out[i])
	}
	return out
}

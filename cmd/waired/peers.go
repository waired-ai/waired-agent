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
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// runPeers dispatches `waired peers <subcommand>`. Only `list` for
// now — the subcommand layer exists so a future `waired peers
// rename` / `peers describe` slots in without breaking the command tree.
const peersLong = `List known mesh peers (DeviceID, IP, engine, GPU, model) so the operator
can pick a '--pin' target for 'worker set'.

  waired peers list [--json]`

func newPeersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "peers",
		// Names the subcommand, like the other namespaces do ("models
		// (ls / pull / rm …)"). The old wording — "List known mesh
		// peers" — read in the top-level index as though the bare command
		// listed them, which it never did (#661).
		Short: "Inspect known mesh peers (list), for picking a 'worker set --pin' target.",
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
	_, _ = fmt.Fprintln(tw, "NAME\tDEVICE-ID\tOVERLAY-IP\tENGINE\tMODEL\tGPU\tVRAM\tMODELS\tWORKER-CAPABLE")
	for _, p := range m.Peers {
		_, _ = fmt.Fprintln(tw, peerRow(p))
	}
	_ = tw.Flush()
	if note := staleNote(m); note != "" {
		_, _ = fmt.Fprint(w, note)
	}
}

// staleNote explains the "stale" reason under the table, and only when a
// row actually says it.
//
// Two separate questions a reader has, neither of which the word answered
// (#661): how old is old enough, and does the row ever go away. A peer that
// had been offline for nine days still listed as `no (stale)`, which reads
// like the listing is broken rather than like the peer is gone.
//
// The threshold comes from the snapshot rather than from
// inferencemesh.DefaultAdvertisedLiveness, so the number printed is the one
// the daemon is actually applying — a constant quoted here would be a guess
// about the other process.
func staleNote(m *inferencemesh.Snapshot) string {
	stale := false
	for _, p := range m.Peers {
		if inferencemesh.PeerCondition(p) == inferencemesh.ConditionStale {
			stale = true
			break
		}
	}
	if !stale {
		return ""
	}
	window := time.Duration(m.StalenessThresholdMS) * time.Millisecond
	if window <= 0 {
		window = inferencemesh.DefaultAdvertisedLiveness
	}
	return fmt.Sprintf(
		"\n\"stale\" means the peer's own last report was more than %s old when this\n"+
			"device's network map was updated. Peers stay listed until they are removed\n"+
			"from your network, however long they have been offline.\n", window)
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
			// #662: the summary-level usable bound wins on a host that
			// shares RAM with its GPU, because Apple Silicon publishes no
			// per-device total and this column read "-" for an M-series
			// Mac while an AMD Strix Halo beside it showed 96 GB. The
			// fallback rule is signer.HardwareSummary's own; hostfit owns
			// the single implementation of it.
			if mb := hostfit.FromHardwareSummary(hw).EffectiveVRAMMB(); mb > 0 {
				vram = fmt.Sprintf("%d GB", (mb+512)/1024)
			}
		}
		if len(p.InferenceState.Models) > 0 {
			models = strings.Join(p.InferenceState.Models, ",")
		}
	}
	// MODEL is the catalog model_id where the peer reports one, so the
	// column is comparable across a mixed fleet; MODELS keeps the engine
	// tags, which are what a request is matched against and therefore
	// what to check when routing surprises you (waired#1064).
	model := "-"
	if m := inferencemesh.PeerModel(p); m != "" {
		model = m
	}
	capable := "yes"
	if !inferencemesh.PeerServing(p) {
		capable = "no (" + capableReason(inferencemesh.PeerCondition(p)) + ")"
	}
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
		name, peerDisplayID(p), overlay, engine, model, gpu, vram, models, capable)
}

// capableReason phrases a peer condition for the WORKER-CAPABLE column.
//
// The classification is shared (inferencemesh.PeerCondition) — a fourth
// copy of that predicate is what waired#1064 removed. Only the WORDING
// is local, and only for the three conditions that mean "cannot serve,
// no reason given": a diagnostic table keeps them apart, where a menu
// row collapses them into the one published word ("unavailable") because
// they read the same to a person. These four strings are what this
// column has always printed; everything else is new specificity the peer
// volunteered, phrased the same way the menus phrase it.
func capableReason(c string) string {
	switch c {
	case inferencemesh.ConditionStale:
		return "stale"
	case inferencemesh.ConditionUnreachable:
		return "unreachable"
	case inferencemesh.ConditionUnavailable:
		return "no model"
	case signer.SubsystemStateNoEngine:
		return "no engine"
	}
	return inferencemesh.ConditionLabel(c)
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

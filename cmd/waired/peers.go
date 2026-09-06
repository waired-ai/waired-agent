package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/management"
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
		Short: "Inspect known mesh peers (list), for picking a 'worker set --pin' target",
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
		Short: "Render the mesh-peer snapshot (name, DeviceID, IP, engine, GPU, models, worker-capable)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			snap, err := fetchMeshSnapshot(mgmt, 2*time.Second)
			if err != nil {
				return fmt.Errorf("waired peers list: %w", err)
			}
			if jsonOut {
				return writePeersJSON(stdout, snap)
			}
			writePeersTable(stdout, snap, fetchOwnPathView(mgmt))
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
func writePeersTable(w io.Writer, m *inferencemesh.Snapshot, own *management.Status) {
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
	if note := ownViewNote(m, own); note != "" {
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

// fetchOwnPathView reads this computer's own view of the overlay: which
// peers it has actually heard from, and whether the node key the network
// publishes for it is still the one it holds.
//
// Best-effort by construction. A transport error, a non-200, a body that
// will not decode, or a daemon too old to carry the fields all yield nil
// and the table simply says nothing extra. `waired peers list` worked
// before this reading existed and has to keep working when it fails.
//
// Routed through httpGet — that is, mgmtReadRoute — rather than a client
// of its own. /waired/v1/status happens to sit in the daemon's TCP read
// allow-list today, which is exactly why a hand-rolled client here would
// look correct right up until that list changes (#785).
func fetchOwnPathView(mgmt string) *management.Status {
	body, err := httpGet(mgmtURL(mgmt, "/waired/v1/status"))
	if err != nil {
		return nil
	}
	var st management.Status
	if err := json.Unmarshal(body, &st); err != nil {
		return nil
	}
	return &st
}

// Wording for ownViewNote. Owner-approved 2026-08-19 (#849); the
// docs-site reference quotes these lines, so a change here changes the
// page too.
const (
	ownViewNoteTail = "WORKER-CAPABLE is what each computer reports about itself, not something this\n" +
		"computer checked. Run `waired doctor` to measure this computer's connection.\n"

	noReplyFromSomeNote = "This computer has had no reply from: %s.\n" + ownViewNoteTail
	noReplyFromAnyNote  = "This computer has had no reply from any computer listed above.\n" + ownViewNoteTail

	keyMismatchNote = "This computer's key does not match the one your network has for it, so no other\n" +
		"computer can reach it. Run `waired init` to register this device again.\n"
)

// ownViewNote states, under the table, the part this computer knows and
// the rows cannot say.
//
// Every column above except the stale marker is the peer's claim about
// itself — WORKER-CAPABLE is the peer probing its own engine and pushing
// the verdict onwards. Those claims arrive over the control plane, not
// the data plane, so a computer whose overlay is dead still receives
// them, still lists every peer as capable, and still routes to all of
// them. Every row saying yes is the least useful answer available
// (#849).
//
// Two facts about here, both already published on /waired/v1/status:
//
//   - Whether this device's node key still matches the one the network
//     publishes for it. When it does not, no peer can reach it and
//     nothing it sends can be answered, so that is the whole story and
//     the note says only that. `waired doctor` reports the same
//     condition off the same field (deviceKeyFindingFrom), which is why
//     the two surfaces cannot disagree about whether it holds.
//   - How many disco replies this computer has had from each peer. Zero
//     on both the direct and the relay path means it has never had an
//     answer from that peer since the agent started.
//
// Deliberately NOT a verdict, and deliberately not a filter: a reply is
// evidence the path works, but its absence is not proof the peer is
// unusable, and waired#729 settled that the router must not drop peers
// on this signal. The note points at `waired doctor`, which measures
// (owner ruling 2026-08-12, waired-ai/waired#1137).
//
// Silent whenever the answer would be a guess: no reading at all, a
// daemon carrying no per-peer path state, or disco switched off — where
// the sample counts are structurally zero for every peer and reading
// them as silence would libel a healthy fleet.
func ownViewNote(m *inferencemesh.Snapshot, own *management.Status) string {
	if own == nil {
		return ""
	}
	if own.NodeKeyAgreement == management.NodeKeyAgreementDiverged {
		return "\n" + keyMismatchNote
	}
	if !own.DiscoEnabled || len(own.Peers) == 0 || m == nil || len(m.Peers) == 0 {
		return ""
	}
	// Keyed by the real DeviceID, which both sides carry and neither
	// prints: PeerStatus.DisplayID is empty for a public machine with no
	// pseudonym, and matching on it would silently drop exactly those
	// rows.
	answered := make(map[string]bool, len(own.Peers))
	for _, ps := range own.Peers {
		answered[ps.DeviceID] = ps.DirectSampleCount > 0 || ps.RelaySampleCount > 0
	}
	var quiet []string
	for _, p := range m.Peers {
		heard, known := answered[p.DeviceID]
		if !known || heard {
			continue
		}
		if name := peerNoteName(p); name != "" {
			quiet = append(quiet, name)
		}
	}
	if len(quiet) == 0 {
		return ""
	}
	// Table order, not sorted here: the aggregator already sorts peers by
	// the name this column shows, so the note reads down the table.
	if len(quiet) == len(m.Peers) {
		return "\n" + noReplyFromAnyNote
	}
	return "\n" + fmt.Sprintf(noReplyFromSomeNote, strings.Join(quiet, ", "))
}

// peerNoteName is what the note calls a peer: the same word the NAME
// column shows, so a reader can find the row.
//
// DeviceName is safe to print for a Public Share peer — the control
// plane substitutes the grant pseudonym there at injection time — and
// where there is no name at all the display identifier stands in, never
// the real DeviceID (public share spec §8.5, #739). A public machine
// that has neither is named by its label rather than dropped, since the
// row it refers to is on screen.
func peerNoteName(p inferencemesh.PeerView) string {
	if p.DeviceName != "" {
		return p.DeviceName
	}
	if id := peerDisplayID(p); id != "" {
		return id
	}
	if p.Grant != nil {
		return inferencemesh.PublicPeerLabel
	}
	return ""
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
	deviceID := peerDisplayID(p)
	if deviceID == "" {
		deviceID = "-"
	}
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s",
		name, deviceID, overlay, engine, model, gpu, vram, models, capable)
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
		// "unreachable" read as "this computer cannot get to it", which
		// is not what the condition means: it is the PEER's own probe of
		// its OWN engine coming back empty (signer.InferenceState
		// .Reachable, cmd/waired-agent/inference_probe.go). Whether this
		// computer can get to it is a different fact, and ownViewNote is
		// where it is said (#849).
		return "engine not answering"
	case inferencemesh.ConditionUnavailable:
		return "no model"
	case signer.SubsystemStateNoEngine:
		return "no engine"
	}
	return inferencemesh.ConditionLabel(c)
}

// peerDisplayID is the identifier this listing may show for a peer, or
// "" when there is none to show.
//
// Delegates to inferencemesh.PeerDisplayID so this listing, the tray and
// the management API cannot drift on the answer — the same move
// peerIsServing made for "is this peer serving" (waired#1064). The rule
// is public share spec §8.5: only a public machine's grant pseudonym may
// be displayed, never the real device identifier.
//
// A grant peer with no pseudonym yields "" rather than its DeviceID.
// Both surfaces below already have a word for "nothing to show" — the
// table's "-" and an absent JSON value — and neither of them is a
// stranger's device id (#739).
func peerDisplayID(p inferencemesh.PeerView) string {
	id, ok := inferencemesh.PeerDisplayID(p)
	if !ok {
		return ""
	}
	return id
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
//
// A public machine with no pseudonym leaves device_id empty rather than
// falling back to the real one (#739): a reader that keys off the field
// gets nothing, which is what we have to give.
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

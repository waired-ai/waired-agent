package main

import (
	"context"
	"fmt"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// What goes into this user's Waired /model rows: the fixed directive table,
// minus rows this computer cannot honour, plus one row per computer that is
// serving right now (waired-agent#830), plus a 1M twin wherever a side
// declares a 1M window (owner ruling 2026-09-06).
//
// It runs in the unprivileged CLI child that owns the file, which has no
// daemon handle beyond the management API — so the mesh arrives over the same
// read route `waired peers list` uses, bounded and best-effort. Writing the
// rows must never turn a good `waired claude enable` into a failed one
// (claude_picker_write.go), and at enable time the daemon may legitimately
// have no network map yet, so "no peers" is the ordinary answer rather than a
// fault.

// pickerMeshTimeout bounds the mesh read. Matches `waired peers list`'s own
// budget; the sudo hop around this whole step allows 30s, and spending a
// meaningful slice of that on rows that are an enhancement would be the wrong
// trade.
const pickerMeshTimeout = 2 * time.Second

// pickerModelFacts is what the entry list is computed from — a struct so the
// decision is a pure function of stated facts and every case is a table row,
// rather than something only reproducible with a daemon attached.
type pickerModelFacts struct {
	// localServes is false on a computer with no AI engine of its own AND on
	// one whose operator has turned local inference off. The local row is
	// dropped in both cases: it names an action this machine will not take,
	// and the whole point of the peer rows is that it does not have to
	// (owner ruling 2026-08-20; the inference-off half is waired-agent#1177,
	// found on a real host during rc5 — the row was offered on a machine
	// that answers nothing, and picking it failed every turn).
	localServes bool
	// localWindow1M and peerWindow1M say whether this computer, and any peer
	// that could answer, declares a 1M context window. They gate the "[1m]"
	// twins: the tier is a promise about the SERVING node, so a twin with no
	// node behind it would be a menu entry whose selection fails.
	localWindow1M bool
	peerWindow1M  bool
	peers         []claudecode.PeerFact
	peerLimit     int
	// publicShareOn is the consumer's Public Share posture, consent
	// included — management's EffectiveMode is already "off until a consent
	// record for the current warning text exists", so one field answers both.
	//
	// The public entry is left out when it is false, by owner ruling
	// (2026-08-20, waired-agent#901): offering a row that fails on selection
	// loses to not offering it, because the picker cannot render a row as
	// disabled. A host that consents later picks the entry up at its next
	// `claude` launch, since the SessionStart hook rebuilds this every time.
	publicShareOn bool
}

// pickerModels renders the rows from the facts.
//
// Each row is followed immediately by its 1M twin where one is offered, so
// the two spellings of one destination sit together rather than the twins
// collecting at the bottom of a menu that folds.
func pickerModels(f pickerModelFacts) []claudecode.PickerRow {
	out := make([]claudecode.PickerRow, 0, 2*(len(claudecode.DirectiveModels())+f.peerLimit))
	add := func(d claudecode.DirectiveModel, tier1M bool) {
		out = append(out, claudecode.PickerRow{
			Model: d.ID, Label: d.DisplayName, Description: d.Description,
		})
		if !tier1M {
			return
		}
		t := claudecode.Tier1MModel(d)
		out = append(out, claudecode.PickerRow{
			Model: t.ID, Label: t.DisplayName, Description: t.Description,
		})
	}
	anywhere1M := f.localWindow1M || f.peerWindow1M
	for _, d := range claudecode.DirectiveModels() {
		switch d.ID {
		case claudecode.DirectiveModelLocal:
			if !f.localServes {
				continue
			}
			add(d, f.localWindow1M)
		case claudecode.DirectiveModelPeer:
			add(d, f.peerWindow1M)
		case claudecode.DirectiveModelPublic:
			if !f.publicShareOn {
				continue
			}
			// A public machine is someone else's, and this host learns its
			// window only when it answers. Offering a tier we cannot check
			// would be the menu entry that fails on selection, so the public
			// row has no twin.
			add(d, false)
		default:
			// The any-node row. Waired picks, so a twin is honest as soon as
			// ANY side declares 1M.
			add(d, anywhere1M)
		}
	}
	for _, r := range claudecode.PeerDirectiveModels(f.peers, f.peerLimit) {
		add(r.DirectiveModel, r.Window1M)
	}
	return out
}

// pickerFactsFromSnapshot projects a mesh snapshot into the facts above.
//
// Only serving peers get a row. A row for a computer that cannot answer is a
// menu entry whose selection fails, and the picker cannot render one as
// disabled.
//
// Names come from inferencemesh.PeerDisplayName, so a public machine is named
// by its grant pseudonym and never by its real device name (spec §8.5), and
// one whose pseudonym is missing is dropped by PeerDirectiveModels rather than
// named some other way.
func pickerFactsFromSnapshot(snap *inferencemesh.Snapshot, limit int) pickerModelFacts {
	f := pickerModelFacts{peerLimit: limit}
	if snap == nil {
		// No answer from the daemon. Assume this computer serves — the fixed
		// table is what every host had before per-peer rows existed, and
		// dropping the local row on a failed READ would turn a transient into
		// a missing menu item. No 1M twins, though: a twin is a claim about a
		// declared window, and there is nothing here that declared one.
		f.localServes = true
		return f
	}
	f.localServes = localServes(snap.Self)
	f.localWindow1M = f.localServes && declares1M(snap.Self)
	if limit <= 0 {
		return f
	}
	for _, p := range snap.Peers {
		if !inferencemesh.PeerServing(p) {
			continue
		}
		name, ok := inferencemesh.PeerDisplayName(p)
		if !ok {
			continue
		}
		wide := declares1M(p)
		f.peerWindow1M = f.peerWindow1M || wide
		f.peers = append(f.peers, claudecode.PeerFact{
			DisplayID: name,
			Model:     inferencemesh.PeerModel(p),
			Window1M:  wide,
		})
	}
	return f
}

// declares1M reports whether a node publishes a 1M input window.
//
// InferenceState.ContextWindow is what the engine is actually loaded with for
// the model it is serving — not the model's native window and not what the
// host could theoretically hold — which is exactly the claim a "[1m]" row
// makes on the operator's behalf. A node that publishes nothing gets no twin.
func declares1M(p inferencemesh.PeerView) bool {
	return p.InferenceState != nil && p.InferenceState.ContextWindow >= hostfit.ServingWindow1M
}

// localServes reports whether this computer will answer a turn of its own.
//
// Two states say no, and they are different states. "No engine at all" is one
// (docs/decisions/20260819/2140-no-engine-is-a-state-not-an-engine.md). The
// other is an operator who turned local inference off, which rc5 found
// offered as a row on a machine that answers nothing: every turn on it failed
// (waired-agent#1177). SubsystemStateDisabled is that intent on the wire, and
// the daemon already sets it ahead of engine health, so reading it here is
// reading the same answer the rest of the fleet sees.
//
// Deliberately NOT keyed on the engine being reachable this second: a stopped
// or restarting engine is still this computer's engine, and dropping the row
// every time it blipped would make the menu flicker.
func localServes(self inferencemesh.PeerView) bool {
	if self.InferenceState == nil {
		return false
	}
	if self.InferenceState.SubsystemState == signer.SubsystemStateDisabled {
		return false
	}
	t := self.InferenceState.Type
	return t != "" && t != "none"
}

// pickerRows resolves the facts for real, reading the mesh over the
// management API. Degrades to the fixed table on any failure, with one warning
// line — the rows are an enhancement and the file must still be written.
func pickerRows(mgmtAddr string, peerLimit int) []claudecode.PickerRow {
	ctx, cancel := context.WithTimeout(context.Background(), pickerMeshTimeout)
	defer cancel()
	snap, err := fetchMeshSnapshotCtx(ctx, mgmtAddr)
	if err != nil {
		fmt.Fprintf(stderr, "Warning: could not read the mesh for /model picker entries (%v); writing the fixed entries only\n", err)
		snap = nil
	}
	f := pickerFactsFromSnapshot(snap, peerLimit)
	f.publicShareOn = publicShareEnabled(mgmtAddr)
	return pickerModels(f)
}

// publicShareEnabled asks the daemon for the consumer's Public Share posture.
//
// A failed read reports false, which leaves the entry out. That is the safe
// direction here and the opposite of the local entry's: a missing local row
// takes away a choice the host could have made, while a public row on a host
// that never consented offers one it must not. Silent, because this runs
// inside the SessionStart hook on every launch and a warning per launch for a
// feature most hosts do not use would be noise — `waired claude status`
// reports what was written.
func publicShareEnabled(mgmtAddr string) bool {
	if mgmtAddr == "" {
		mgmtAddr = defaultMgmtAddr
	}
	var resp management.PublicUseResponse
	if err := publicGetJSON(mgmtAddr, "/waired/v1/public/use", &resp); err != nil {
		return false
	}
	return resp.EffectiveMode != "" && resp.EffectiveMode != agentconfig.PublicUseModeOff
}

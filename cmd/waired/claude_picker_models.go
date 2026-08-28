package main

import (
	"context"
	"fmt"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/management"
)

// What goes into this user's /model picker cache: the fixed directive table,
// minus entries this computer cannot honour, plus one row per computer that is
// serving right now (waired-agent#830).
//
// It runs in the unprivileged CLI child that owns the file, which has no
// daemon handle beyond the management API — so the mesh arrives over the same
// read route `waired peers list` uses, bounded and best-effort. A cache write
// must never turn a good `waired claude enable` into a failed one
// (claude_models_cache.go), and at enable time the daemon may legitimately have
// no network map yet, so "no peers" is the ordinary answer rather than a fault.

// pickerMeshTimeout bounds the mesh read. Matches `waired peers list`'s own
// budget; the sudo hop around this whole step allows 30s, and spending a
// meaningful slice of that on rows that are an enhancement would be the wrong
// trade.
const pickerMeshTimeout = 2 * time.Second

// pickerModelFacts is what the entry list is computed from — a struct so the
// decision is a pure function of stated facts and every case is a table row,
// rather than something only reproducible with a daemon attached.
type pickerModelFacts struct {
	// engineUsable is false on a computer with no AI engine of its own. The
	// local entry is dropped there: it names an action this machine cannot
	// take, and the whole point of the peer entries is that it does not have
	// to (owner ruling 2026-08-20).
	engineUsable bool
	peers        []claudecode.PeerFact
	peerLimit    int
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

// pickerModels renders the cache contents from the facts.
func pickerModels(f pickerModelFacts) []claudecode.GatewayCacheModel {
	out := make([]claudecode.GatewayCacheModel, 0, len(claudecode.DirectiveModels())+f.peerLimit)
	for _, d := range claudecode.DirectiveModels() {
		if d.ID == claudecode.DirectiveModelLocal && !f.engineUsable {
			continue
		}
		if d.ID == claudecode.DirectiveModelPublic && !f.publicShareOn {
			continue
		}
		out = append(out, claudecode.GatewayCacheModel(d))
	}
	for _, d := range claudecode.PeerDirectiveModels(f.peers, f.peerLimit) {
		out = append(out, claudecode.GatewayCacheModel(d))
	}
	return out
}

// pickerFactsFromSnapshot projects a mesh snapshot into the facts above.
//
// Only serving peers get a row. A row for a computer that cannot answer is a
// menu entry whose selection fails, and the picker has no way to grey one out
// — every gateway-supplied row renders identically, with a hard-coded "From
// gateway" and nothing else (measured; see the knowledge note).
//
// Names come from inferencemesh.PeerDisplayName, so a public machine is named
// by its grant pseudonym and never by its real device name (spec §8.5), and
// one whose pseudonym is missing is dropped by PeerDirectiveModels rather than
// named some other way.
func pickerFactsFromSnapshot(snap *inferencemesh.Snapshot, limit int) pickerModelFacts {
	f := pickerModelFacts{peerLimit: limit}
	if snap == nil {
		// No answer from the daemon. Assume this computer can serve — the
		// fixed table is what every host had before per-peer rows existed,
		// and dropping the local entry on a failed READ would turn a
		// transient into a missing menu item.
		f.engineUsable = true
		return f
	}
	f.engineUsable = localEngineUsable(snap.Self)
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
		f.peers = append(f.peers, claudecode.PeerFact{
			DisplayID: name,
			Model:     inferencemesh.PeerModel(p),
		})
	}
	return f
}

// localEngineUsable reports whether this computer has an engine of its own
// that could answer.
//
// Keyed on the engine being present and named, not on it being reachable this
// second: a stopped or restarting engine is still this computer's engine, and
// removing the entry every time the engine blipped would make the menu
// flicker. "no engine at all" is the state the entry is wrong for
// (docs/decisions/20260819/2140-no-engine-is-a-state-not-an-engine.md).
func localEngineUsable(self inferencemesh.PeerView) bool {
	if self.InferenceState == nil {
		return false
	}
	t := self.InferenceState.Type
	return t != "" && t != "none"
}

// pickerCacheModels resolves the facts for real, reading the mesh over the
// management API. Degrades to the fixed table on any failure, with one warning
// line — the rows are an enhancement and the file must still be written.
func pickerCacheModels(mgmtAddr string, peerLimit int) []claudecode.GatewayCacheModel {
	ctx, cancel := context.WithTimeout(context.Background(), pickerMeshTimeout)
	defer cancel()
	snap, err := fetchMeshSnapshotCtx(ctx, mgmtAddr)
	if err != nil {
		fmt.Fprintf(stderr, "warning: could not read the mesh for /model picker entries (%v); writing the fixed entries only\n", err)
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

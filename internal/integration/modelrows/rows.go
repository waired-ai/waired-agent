// Package modelrows builds the Waired rows a coding tool's model list shows:
// the fixed route directives this computer can honour, plus one row per
// computer that is serving on the mesh right now.
//
// It exists because two callers need the same answer from the same mesh
// snapshot and used to be able to drift apart. `waired claude _picker` runs
// in the unprivileged CLI and reads the mesh over the management API; the
// daemon answers GET /v1/models on the Local Gateway and holds the snapshot
// directly (waired-agent#1306). The projection from a snapshot to rows is the
// part that must agree, so it lives here once.
//
// The rows are the same on both surfaces, "[1m]" twins and stated windows
// included (owner decision 2026-09-16, waired-agent#1395). What each caller
// still owns is how a row is RENDERED: Claude Code's writer puts the id and
// the two lines into modelPicker, and the OpenAI-dialect listing adds the
// window as max_input_tokens.
package modelrows

import (
	"github.com/waired-ai/waired-agent/internal/inferencemesh"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// Facts is what the row list is computed from — a struct so the decision is a
// pure function of stated facts and every case is a table row, rather than
// something only reproducible with a daemon attached.
type Facts struct {
	// LocalServes is false on a computer with no AI engine of its own AND on
	// one whose operator has turned local inference off. The local row is
	// dropped in both cases: it names an action this machine will not take,
	// and the whole point of the peer rows is that it does not have to
	// (owner ruling 2026-08-20; the inference-off half is waired-agent#1177,
	// found on a real host during rc5 — the row was offered on a machine
	// that answers nothing, and picking it failed every turn).
	LocalServes bool
	// LocalWindow is the input window this computer's engine is loaded with,
	// 0 when it has not said. It is the honest number for the local row.
	LocalWindow int
	// LocalWindow1M, PeerWindow1M and PublicWindow1M say whether this
	// computer, one of this operator's other computers (a teammate's
	// included), and a public machine this computer may use declare a 1M
	// context window. They gate the "[1m]" twins: the tier is a promise about
	// the SERVING node, so a twin with no node behind it would be a menu
	// entry whose selection fails.
	LocalWindow1M  bool
	PeerWindow1M   bool
	PublicWindow1M bool
	Peers          []claudecode.PeerFact
	PeerLimit      int
	// PublicShareOn is the consumer's Public Share posture, consent
	// included — management's EffectiveMode is already "off until a consent
	// record for the current warning text exists", so one field answers both.
	//
	// The public entry is left out when it is false, by owner ruling
	// (2026-08-20, waired-agent#901): offering a row that fails on selection
	// loses to not offering it, because a picker cannot render a row as
	// disabled. A host that consents later picks the entry up the next time
	// its client asks.
	PublicShareOn bool
}

// Row is one row of a model list: the id a turn carries, the two lines the
// row shows, and the window it states.
type Row struct {
	claudecode.DirectiveModel
	// Window1M is whether the side this row names declares a 1M window. A row
	// with it set is followed by its twin.
	Window1M bool
	// Tier1M marks the "[1m]" twin itself.
	Tier1M bool
	// ContextWindow is the window the row states, and it is exactly what
	// routing guarantees for it (gateway.RequiredWindowFor):
	//
	//   - a twin: 1M, the floor its id carries;
	//   - a row where Waired chooses the computer (any, peer, public): 200k,
	//     the floor those ids carry;
	//   - a row naming one computer (local, per-computer): that computer's
	//     declared window, 0 when it declares none — those rows carry no
	//     floor, so the machine's own figure is the only honest one.
	//
	// It used to be 0 on every row not naming one machine, and the
	// OpenAI-dialect listing filled that 0 with the REQUESTING computer's
	// window, which nothing enforced (waired-agent#1395).
	ContextWindow int
}

// Rows renders the row list from the facts.
//
// Order is the fixed table first, in DirectiveModels' order, then the
// per-peer rows, and each twin immediately after its row, so the two
// spellings of one destination sit together rather than the twins collecting
// at the bottom of a menu that folds.
func Rows(f Facts) []Row {
	out := make([]Row, 0, 2*(len(claudecode.DirectiveModels())+f.PeerLimit))
	add := func(r Row) {
		out = append(out, r)
		if !r.Window1M {
			return
		}
		out = append(out, Row{
			DirectiveModel: claudecode.Tier1MModel(r.DirectiveModel),
			Window1M:       true,
			Tier1M:         true,
			ContextWindow:  hostfit.ServingWindow1M,
		})
	}
	for _, d := range claudecode.DirectiveModels() {
		switch d.ID {
		case claudecode.DirectiveModelLocal:
			if !f.LocalServes {
				continue
			}
			add(Row{DirectiveModel: d, Window1M: f.LocalWindow1M, ContextWindow: f.LocalWindow})
		case claudecode.DirectiveModelPeer:
			add(Row{DirectiveModel: d, Window1M: f.PeerWindow1M, ContextWindow: hostfit.ServingWindow200k})
		case claudecode.DirectiveModelPublic:
			if !f.PublicShareOn {
				continue
			}
			add(Row{DirectiveModel: d, Window1M: f.PublicWindow1M, ContextWindow: hostfit.ServingWindow200k})
		default:
			// The any-node row. It routes to this operator's own computers,
			// and to a public machine only when the Public Share posture's
			// own comparison admits one, so only this computer and the
			// operator's other computers can make its twin honest.
			add(Row{DirectiveModel: d, Window1M: f.LocalWindow1M || f.PeerWindow1M, ContextWindow: hostfit.ServingWindow200k})
		}
	}
	for _, r := range claudecode.PeerDirectiveModels(f.Peers, f.PeerLimit) {
		add(Row{DirectiveModel: r.DirectiveModel, Window1M: r.Window1M, ContextWindow: r.ContextWindow})
	}
	return out
}

// FactsFromSnapshot projects a mesh snapshot into the facts above.
//
// Only serving peers get a row. A row for a computer that cannot answer is a
// menu entry whose selection fails, and a picker cannot render one as
// disabled. The others are still in Peers, marked NotServing, because their
// names decide which ids need a hash — and PeerForDirective, which resolves
// an id against every peer, has to reach the same ids.
//
// Names come from inferencemesh.PeerDisplayName, so a public machine is named
// by its grant pseudonym and never by its real device name (spec §8.5), and
// one whose pseudonym is missing is left out rather than named some other
// way.
//
// A public machine counts as serving only while Public Share is on. A grant
// can outlive the posture by its TTL, and a row for it is refused by the
// Public Share gate — and, before waired-agent#1395, then quietly answered by
// one of the operator's own computers.
//
// A peer counts toward a 1M twin only when it could answer that twin's main
// conversation: serving, declaring 1M, and not switched off for main
// conversations by its owner. The listing cannot know which kind of turn will
// arrive, and a picker row is what the main conversation is sent on.
//
// publicShareOn is a separate argument because it is not in the snapshot:
// both callers read it from management, and neither can derive it here.
func FactsFromSnapshot(snap *inferencemesh.Snapshot, limit int, publicShareOn bool) Facts {
	f := Facts{PeerLimit: limit, PublicShareOn: publicShareOn}
	if snap == nil {
		// No answer. Assume this computer serves — the fixed table is what
		// every host had before per-peer rows existed, and dropping the local
		// row on a failed READ would turn a transient into a missing menu
		// item. No 1M twins, though: a twin is a claim about a declared
		// window, and there is nothing here that declared one.
		f.LocalServes = true
		return f
	}
	f.LocalServes = LocalServes(snap.Self)
	if f.LocalServes {
		f.LocalWindow = declaredWindow(snap.Self)
		f.LocalWindow1M = f.LocalWindow >= hostfit.ServingWindow1M
	}
	peers, from := peerFacts(snap.Peers)
	for i := range peers {
		pv := snap.Peers[from[i]]
		public := inferencemesh.IsPublicGrant(pv.Grant)
		if public && !publicShareOn {
			peers[i].NotServing = true
		}
		if pv.InferenceState != nil && pv.InferenceState.ExcludeMain {
			peers[i].Window1M = false
		}
		if peers[i].NotServing || !peers[i].Window1M {
			continue
		}
		if public {
			f.PublicWindow1M = true
		} else {
			f.PeerWindow1M = true
		}
	}
	// The 1M facts above are about every peer, whatever the per-computer row
	// limit: with the limit at 0 the peer twins used to disappear with the
	// per-computer rows (waired-agent#1395).
	if limit > 0 {
		f.Peers = peers
	}
	return f
}

// peerFacts is the one projection from mesh peers to PeerFacts, shared by the
// rows and by PeerForDirective so the two cannot come to disagree about an
// id. from[i] is the index in peers that fact i came from.
func peerFacts(peers []inferencemesh.PeerView) (facts []claudecode.PeerFact, from []int) {
	for i, p := range peers {
		name, ok := inferencemesh.PeerDisplayName(p)
		if !ok {
			continue
		}
		win := declaredWindow(p)
		facts = append(facts, claudecode.PeerFact{
			DisplayID:     name,
			Key:           peerKey(p),
			NotServing:    !inferencemesh.PeerServing(p),
			Model:         inferencemesh.PeerModel(p),
			Window1M:      win >= hostfit.ServingWindow1M,
			ContextWindow: win,
		})
		from = append(from, i)
	}
	return facts, from
}

// peerKey is what a hashed id is derived from. The device id for one of your
// own computers and for a teammate's. For a public machine it is the grant id:
// a hash of the device id would be a handle on a stranger's machine that
// outlives the grant, which is what the pseudonym exists to avoid (spec §8.5).
func peerKey(p inferencemesh.PeerView) string {
	if p.Grant != nil && !inferencemesh.IsTeamGrant(p.Grant) {
		return p.Grant.ID
	}
	return p.DeviceID
}

// PeerForDirective is the peer a per-peer directive id names, found by
// generating every peer's id from these peers and matching the id in full.
//
// It looks at every peer, serving or not. An id that names a computer which
// has stopped answering still names that computer, so the turn is pinned to
// it and fails with the reason it cannot serve, rather than as if the
// computer had left.
func PeerForDirective(peers []inferencemesh.PeerView, id string) (inferencemesh.PeerView, bool) {
	facts, from := peerFacts(peers)
	for i, got := range claudecode.PeerDirectiveIDs(facts) {
		if got != "" && got == id {
			return peers[from[i]], true
		}
	}
	return inferencemesh.PeerView{}, false
}

// declaredWindow is the input window a node publishes, 0 when it publishes
// none.
//
// InferenceState.ContextWindow is what the engine is actually loaded with for
// the model it is serving — not the model's native window and not what the
// host could theoretically hold — which is exactly the claim a row makes on
// the operator's behalf.
func declaredWindow(p inferencemesh.PeerView) int {
	if p.InferenceState == nil {
		return 0
	}
	return p.InferenceState.ContextWindow
}

// LocalServes reports whether this computer will answer a turn of its own.
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
func LocalServes(self inferencemesh.PeerView) bool {
	if self.InferenceState == nil {
		return false
	}
	if self.InferenceState.SubsystemState == signer.SubsystemStateDisabled {
		return false
	}
	t := self.InferenceState.Type
	return t != "" && t != "none"
}

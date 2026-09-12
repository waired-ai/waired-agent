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
// What each caller still owns is how a row is RENDERED. Claude Code sizes a
// session from a "[1m]" suffix in the id, so its writer adds twins; the
// OpenAI-dialect surface says the same thing in max_input_tokens and adds
// none.
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
	// LocalWindow1M and PeerWindow1M say whether this computer, and any peer
	// that could answer, declares a 1M context window. They gate the "[1m]"
	// twins a caller may add: the tier is a promise about the SERVING node,
	// so a twin with no node behind it would be a menu entry whose selection
	// fails.
	LocalWindow1M bool
	PeerWindow1M  bool
	Peers         []claudecode.PeerFact
	PeerLimit     int
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
// row shows, and the two numbers a renderer may want.
type Row struct {
	claudecode.DirectiveModel
	// Window1M is whether the side this row names declares a 1M window.
	Window1M bool
	// ContextWindow is the input window the side this row names is actually
	// loaded with, or 0 when this computer cannot know it — which is every
	// row that does not name one machine, because which machine answers is
	// decided per turn.
	ContextWindow int
}

// Rows renders the row list from the facts.
//
// Order is the fixed table first, in DirectiveModels' order, then the
// per-peer rows. A caller that adds twins puts each one immediately after its
// row, so the two spellings of one destination sit together rather than the
// twins collecting at the bottom of a menu that folds.
func Rows(f Facts) []Row {
	out := make([]Row, 0, len(claudecode.DirectiveModels())+f.PeerLimit)
	anywhere1M := f.LocalWindow1M || f.PeerWindow1M
	for _, d := range claudecode.DirectiveModels() {
		switch d.ID {
		case claudecode.DirectiveModelLocal:
			if !f.LocalServes {
				continue
			}
			out = append(out, Row{DirectiveModel: d, Window1M: f.LocalWindow1M, ContextWindow: f.LocalWindow})
		case claudecode.DirectiveModelPeer:
			out = append(out, Row{DirectiveModel: d, Window1M: f.PeerWindow1M})
		case claudecode.DirectiveModelPublic:
			if !f.PublicShareOn {
				continue
			}
			// A public machine is someone else's, and this host learns its
			// window only when it answers. Offering a tier we cannot check
			// would be the menu entry that fails on selection, so the public
			// row has no twin.
			out = append(out, Row{DirectiveModel: d})
		default:
			// The any-node row. Waired picks, so a twin is honest as soon as
			// ANY side declares 1M.
			out = append(out, Row{DirectiveModel: d, Window1M: anywhere1M})
		}
	}
	for _, r := range claudecode.PeerDirectiveModels(f.Peers, f.PeerLimit) {
		out = append(out, Row{DirectiveModel: r.DirectiveModel, Window1M: r.Window1M, ContextWindow: r.ContextWindow})
	}
	return out
}

// FactsFromSnapshot projects a mesh snapshot into the facts above.
//
// Only serving peers get a row. A row for a computer that cannot answer is a
// menu entry whose selection fails, and a picker cannot render one as
// disabled.
//
// Names come from inferencemesh.PeerDisplayName, so a public machine is named
// by its grant pseudonym and never by its real device name (spec §8.5), and
// one whose pseudonym is missing is dropped by PeerDirectiveModels rather
// than named some other way.
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
		win := declaredWindow(p)
		wide := win >= hostfit.ServingWindow1M
		f.PeerWindow1M = f.PeerWindow1M || wide
		f.Peers = append(f.Peers, claudecode.PeerFact{
			DisplayID:     name,
			Model:         inferencemesh.PeerModel(p),
			Window1M:      wide,
			ContextWindow: win,
		})
	}
	return f
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

package claudecode

import (
	"strconv"
	"strings"
)

// Per-peer /model entries (waired-agent#830). Alongside the fixed directive
// table in directives.go, the picker can carry one row per computer that is
// currently serving, so an operator can send a conversation to a named machine
// from the menu they are already in.
//
// Two measurements from a rendered picker on a real host shape everything here
// (docs/knowledges/20260820/0300-model-picker-measured-on-device.md, corrected
// on 2026-09-06 against Claude Code 2.1.261):
//
//   - A `modelPicker` row HAS a description, and the picker renders it as the
//     row's second line. The first measurement was of the private cache the
//     rows used to be written into, where every row read "From gateway" and
//     the node and the model both had to fit the label. They no longer do:
//     the node names the row, the model describes it.
//   - The picker folds past about ten rows, six of which are Claude Code's own.
//     So these rows are capped rather than unbounded: an operator with a large
//     fleet is better served by the tray's pin submenu, which scrolls properly.
//
// This package stays free of internal/inferencemesh on purpose — the `waired`
// CLI links it, and the whole point of directives.go's duplication is to keep
// that link light. The caller projects a snapshot into PeerFacts.

const (
	// PeerDirectivePrefix is what a per-peer id starts with. It shares
	// DirectiveModelPeer's spelling so the intercept can recognise the whole
	// family by prefix without a mesh of its own.
	PeerDirectivePrefix = DirectiveModelPeer + "-"

	// peerSlugMaxBytes bounds the generated id. Long enough for a hostname,
	// short enough that the id is not the reason a label wraps. ASCII by
	// construction below, so bytes and characters agree.
	peerSlugMaxBytes = 32
)

// PeerFact is one mesh peer as the picker needs it: what it may be CALLED, and
// what it is running.
//
// DisplayID is the identifier a surface is allowed to show — a device name for
// one of your own machines, a grant pseudonym for a public one. "" means there
// is nothing showable, and the peer is skipped rather than named some other
// way (public share spec §8.5). Resolving that is the caller's job, through
// inferencemesh's display helpers, so this package can stay a pure function of
// plain strings.
type PeerFact struct {
	DisplayID string
	// Model is the catalog model_id the peer is committed to serving, or ""
	// when it names none.
	Model string
	// Window1M is whether the peer declares a 1M input window. It gates that
	// peer's "[1m]" twin: the tier is a promise about the serving node, so a
	// twin offered where the node cannot keep it is a menu entry whose
	// selection fails.
	Window1M bool
}

// PeerDirectiveRow is one per-peer row and whether it gets a 1M twin. The
// flag rides with the row rather than being looked up again by the caller:
// duplicate names get an ordinal on the slug here, so the id the caller sees
// is not always derivable from the display name it started with.
type PeerDirectiveRow struct {
	DirectiveModel
	Window1M bool
}

// PeerDirectiveSlug reduces a display identifier to the id-safe form used in a
// per-peer directive id, or "" when nothing usable survives.
//
// Every rune outside [a-z0-9] becomes a hyphen rather than being dropped. That
// is what makes "mac-mini.local" and "mac-mini" distinct ids: stripping the
// suffix instead would collapse two real machines onto one entry, and the
// fleet that has both is exactly the fleet where it matters.
func PeerDirectiveSlug(displayID string) string {
	var b strings.Builder
	b.Grow(len(displayID))
	prevHyphen := false
	for _, r := range strings.ToLower(displayID) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if !ok {
			r = '-'
		}
		if r == '-' {
			if prevHyphen || b.Len() == 0 {
				continue
			}
			prevHyphen = true
		} else {
			prevHyphen = false
		}
		if b.Len()+1 > peerSlugMaxBytes {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), "-")
}

// PeerDirectiveID is the /model id for a peer, or "" when it cannot be named.
func PeerDirectiveID(displayID string) string {
	slug := PeerDirectiveSlug(displayID)
	if slug == "" {
		return ""
	}
	return PeerDirectivePrefix + slug
}

// IsPeerDirectiveID reports whether id is a per-peer directive id.
//
// A prefix test, not a lookup: the ids are generated from a live snapshot, so
// the layers that only need to know "is this one of ours" (the intercept's
// route decision) cannot enumerate them.
func IsPeerDirectiveID(id string) bool {
	return strings.HasPrefix(id, PeerDirectivePrefix) && len(id) > len(PeerDirectivePrefix)
}

// PeerDirectiveModels renders up to limit peers as picker entries, in the order
// given. limit <= 0 returns none.
//
// Order is the caller's, and the mesh snapshot is already sorted by device
// name, which is what keeps a machine on the same row from one launch to the
// next. Two peers whose names slug identically get an ordinal — "-2", "-3" —
// on both the id and the label, so the entries stay distinguishable and the
// mapping stays deterministic for the same input.
//
// The label names the node because that is the choice being made; the model
// is what makes it a useful choice, so it goes on the description line. Same
// order the tray's pin submenu reads in, so one machine reads the same on
// both surfaces.
func PeerDirectiveModels(peers []PeerFact, limit int) []PeerDirectiveRow {
	if limit <= 0 {
		return nil
	}
	out := make([]PeerDirectiveRow, 0, limit)
	seen := map[string]int{}
	for _, p := range peers {
		if len(out) >= limit {
			break
		}
		slug := PeerDirectiveSlug(p.DisplayID)
		if slug == "" {
			// Nothing showable, or a name that reduces to nothing at all.
			// Offering it as an unnamed row would be a menu entry the
			// operator cannot tell apart from any other.
			continue
		}
		name := p.DisplayID
		seen[slug]++
		if n := seen[slug]; n > 1 {
			ord := strconv.Itoa(n)
			slug = slug + "-" + ord
			name = name + " (" + ord + ")"
		}
		desc := "Another of your computers"
		if p.Model != "" {
			desc = p.Model
		}
		out = append(out, PeerDirectiveRow{
			DirectiveModel: DirectiveModel{
				ID:          PeerDirectivePrefix + slug,
				DisplayName: "Waired peer: " + name,
				Description: desc,
			},
			Window1M: p.Window1M,
		})
	}
	return out
}

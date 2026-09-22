package claudecode

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf8"
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

	// peerKeyHashHex is how many hex digits of a peer's key hash end an id
	// that needs telling apart (see PeerDirectiveIDs).
	peerKeyHashHex = 6
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
	// Key is what tells this peer apart from any other with a similar
	// DisplayID: something stable per machine that the caller already holds.
	// It is never rendered. When an id needs disambiguating, only a short
	// hash of it appears (see PeerDirectiveIDs).
	Key string
	// NotServing is true for a peer that is on the mesh but cannot answer
	// right now. It gets no row, since a picker cannot render a row as
	// disabled. Its name still counts when deciding which ids need a hash,
	// so the resolver, which sees every peer, reaches the same ids as the
	// rows did.
	NotServing bool
	// Model is the catalog model_id the peer is committed to serving, or ""
	// when it names none.
	Model string
	// Window1M is whether the peer declares a 1M input window. It gates that
	// peer's "[1m]" twin: the tier is a promise about the serving node, so a
	// twin offered where the node cannot keep it is a menu entry whose
	// selection fails.
	Window1M bool
	// ContextWindow is the input window the peer declares, 0 when it
	// publishes none — or, for a peer serving a custom model below 200,704
	// tokens, that model's own window (waired-ai/waired#1481). A row states
	// only the second: every other Waired row is a 200k or a 1M session
	// whatever computer answers (waired-agent#1396). It also rides here for
	// the caller that decides whether the peer can take a row at all.
	ContextWindow int
}

// PeerDirectiveRow is one per-peer row, whether it gets a 1M twin, and the
// window the peer declared. They ride with the row rather than being looked
// up again by the caller: an id may end in a hash of the peer's Key (see
// PeerDirectiveIDs), so the id the caller sees is not always derivable from
// the display name it started with.
type PeerDirectiveRow struct {
	DirectiveModel
	Window1M      bool
	ContextWindow int
}

// PeerDirectiveSlug reduces a display identifier to the id-safe form used in a
// per-peer directive id, or "" when nothing usable survives.
//
// Every rune outside [a-z0-9] becomes a hyphen rather than being dropped. That
// is what makes "mac-mini.local" and "mac-mini" distinct ids: stripping the
// suffix instead would collapse two real machines onto one entry, and the
// fleet that has both is exactly the fleet where it matters.
func PeerDirectiveSlug(displayID string) string {
	slug, _ := reduceDisplayID(displayID, peerSlugMaxBytes)
	return slug
}

// reduceDisplayID is PeerDirectiveSlug with the cap as a parameter, and
// whether the cap cut anything off.
func reduceDisplayID(displayID string, maxBytes int) (slug string, cut bool) {
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
		if b.Len()+1 > maxBytes {
			// Only a letter or digit past the cap is lost name; a hyphen
			// there would have been trimmed anyway.
			cut = cut || r != '-'
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), "-"), cut
}

// PeerDirectiveID is the /model id for a peer whose name needs no telling
// apart from another's, or "" when it cannot be named. Ids for a whole
// snapshot come from PeerDirectiveIDs, which may add a hash.
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

// PeerDirectiveIDs is the per-peer id of each peer, index for index, with ""
// for a peer that cannot be named.
//
// An id is what a turn carries back, possibly long after the list was
// written: Claude Code keeps the picker rows with no expiry, and a coding
// tool's config keeps the ids it was given. The resolver regenerates the ids
// from the snapshot it has at that moment and pins the peer whose id matches
// in full. So an id must name one computer, and must not come to name a
// different one while that one is away.
//
// A plain slug does that for most names, and keeps the id a person can read:
// "linux-gpu" stays waired/peer-linux-gpu. It is not enough in three cases,
// and each of those ids ends in a short hash of the peer's Key instead:
//
//   - The name has characters outside ASCII. The slug drops them, so
//     "studio-mac (田中)" and "studio-mac (佐藤)" would both be studio-mac,
//     and so would your own studio-mac.
//   - The cap cut letters or digits off. A teammate label that carries an
//     email address is long, and two of them can share the first 32 bytes.
//   - Two peers in the list reduce to the same slug. Both ids get the hash,
//     not just the second, so no id still means "whichever comes first".
//     The hash replaces the old "-2" ordinal, which named a position in the
//     list rather than a machine.
//
// A hash needs a Key. A peer that needs one and has none gets no id, and so
// does any id two peers still share: an id that could be either machine
// names neither.
func PeerDirectiveIDs(peers []PeerFact) []string {
	slugs := make([]string, len(peers))
	count := map[string]int{}
	for i, p := range peers {
		slugs[i] = PeerDirectiveSlug(p.DisplayID)
		if slugs[i] != "" {
			count[slugs[i]]++
		}
	}
	ids := make([]string, len(peers))
	for i, p := range peers {
		slug := slugs[i]
		if slug == "" {
			// Nothing showable, or a name that reduces to nothing at all.
			// Offering it as an unnamed row would be a menu entry the
			// operator cannot tell apart from any other.
			continue
		}
		_, cut := reduceDisplayID(p.DisplayID, peerSlugMaxBytes)
		if count[slug] > 1 || cut || !isASCII(p.DisplayID) {
			if p.Key == "" {
				continue
			}
			base, _ := reduceDisplayID(p.DisplayID, peerSlugMaxBytes-1-peerKeyHashHex)
			sum := sha256.Sum256([]byte(p.Key))
			slug = base + "-" + hex.EncodeToString(sum[:])[:peerKeyHashHex]
		}
		ids[i] = PeerDirectivePrefix + slug
	}
	uses := map[string]int{}
	for _, id := range ids {
		uses[id]++
	}
	for i, id := range ids {
		if id != "" && uses[id] > 1 {
			ids[i] = ""
		}
	}
	return ids
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// PeerDirectiveModels renders up to limit serving peers as picker entries, in
// the order given. limit <= 0 returns none.
//
// Order is the caller's, and the mesh snapshot is already sorted by device
// name, which is what keeps a machine on the same row from one launch to the
// next. The ids come from PeerDirectiveIDs over every peer given, serving or
// not. Two rows with the same name also get an ordinal on the label — "(2)",
// "(3)" — so a person can tell them apart. The ordinal is only on the label;
// the id is what the turn carries, and it names the machine.
//
// The label names the node because that is the choice being made; the model
// is what makes it a useful choice, so it goes on the description line. Same
// order the tray's pin submenu reads in, so one machine reads the same on
// both surfaces.
func PeerDirectiveModels(peers []PeerFact, limit int) []PeerDirectiveRow {
	if limit <= 0 {
		return nil
	}
	ids := PeerDirectiveIDs(peers)
	out := make([]PeerDirectiveRow, 0, limit)
	seen := map[string]int{}
	for i, p := range peers {
		if len(out) >= limit {
			break
		}
		if ids[i] == "" || p.NotServing {
			continue
		}
		name := p.DisplayID
		seen[name]++
		if n := seen[name]; n > 1 {
			name = name + " (" + strconv.Itoa(n) + ")"
		}
		desc := "Another of your computers"
		if p.Model != "" {
			desc = p.Model
		}
		out = append(out, PeerDirectiveRow{
			DirectiveModel: DirectiveModel{
				ID:          ids[i],
				DisplayName: "Waired peer: " + name,
				Description: desc,
			},
			Window1M:      p.Window1M,
			ContextWindow: p.ContextWindow,
		})
	}
	return out
}

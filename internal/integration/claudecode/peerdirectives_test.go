package claudecode

import (
	"regexp"
	"strings"
	"testing"
)

// PIN: record of the measured picker behaviour
// (docs/knowledges/20260820/0300-model-picker-measured-on-device.md), not a
// ratified string contract — the label shape was approved by the owner
// (waired-agent#830) but nothing fixes the slug rules beyond "produce a
// distinct, stable, id-safe name per machine".
func TestPeerDirectiveSlug(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"a plain hostname passes through", "linux-gpu", "linux-gpu"},
		{"case is folded", "Studio-Mac", "studio-mac"},
		// The case this rule exists for: dropping the suffix instead would
		// give a fleet holding both machines one entry for two.
		{"a .local suffix becomes part of the name", "mac-mini.local", "mac-mini-local"},
		{"and stays distinct from the bare name", "mac-mini", "mac-mini"},
		{"spaces and punctuation become one hyphen", "Bob's  Desktop (spare)", "bob-s-desktop-spare"},
		{"runs of hyphens collapse", "a---b", "a-b"},
		{"leading and trailing hyphens go", "-x-", "x"},
		{"a name that is all punctuation has nothing to show", "!!!", ""},
		{"a non-ASCII name has nothing to show", "作業用", ""},
		{"an empty name has nothing to show", "", ""},
		{
			"an over-long name is cut without a trailing hyphen",
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbb", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := PeerDirectiveSlug(tc.in)
			if got != tc.want {
				t.Errorf("PeerDirectiveSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if len(got) > peerSlugMaxBytes {
				t.Errorf("slug %q is %d bytes, over the %d cap", got, len(got), peerSlugMaxBytes)
			}
			if strings.HasPrefix(got, "-") || strings.HasSuffix(got, "-") {
				t.Errorf("slug %q has a bare hyphen at an end", got)
			}
		})
	}
}

// Every generated id must be recognisable by the layers that cannot enumerate
// them.
func TestPeerDirectiveIDIsRecognisable(t *testing.T) {
	id := PeerDirectiveID("linux-gpu")
	if id != "waired/peer-linux-gpu" {
		t.Fatalf("PeerDirectiveID = %q", id)
	}
	if !IsPeerDirectiveID(id) {
		t.Error("a generated id must be recognised by IsPeerDirectiveID")
	}
	if !IsWairedModelID(id) {
		t.Errorf("id %q is not recognisable as one of ours", id)
	}
	if PeerDirectiveID("!!!") != "" {
		t.Error("an unnamable peer must yield no id at all")
	}
	// The mode entry itself is not a per-peer id, or selecting "any peer"
	// would be read as naming a machine called "".
	if IsPeerDirectiveID(DirectiveModelPeer) {
		t.Error("the bare peer directive must not read as a per-peer id")
	}
	if IsPeerDirectiveID(PeerDirectivePrefix) {
		t.Error("the bare prefix names no peer")
	}
	if IsPeerDirectiveID(DirectiveModelLocal) {
		t.Error("the local pin must not read as a per-peer id")
	}
}

func TestPeerDirectiveModels(t *testing.T) {
	t.Run("node first, then the model it is running", func(t *testing.T) {
		got := PeerDirectiveModels([]PeerFact{{DisplayID: "linux-gpu", Model: "qwen3.5-35b-a3b"}}, 5)
		if len(got) != 1 {
			t.Fatalf("got %d entries, want 1", len(got))
		}
		if got[0].ID != "waired/peer-linux-gpu" {
			t.Errorf("id = %q", got[0].ID)
		}
		// The node names the row and the model describes it. They used to
		// share the label because the private cache the rows were written
		// into had no description field; a `modelPicker` row does, and the
		// picker renders it as the second line (waired-agent#1185).
		if got[0].DisplayName != "Waired peer: linux-gpu" {
			t.Errorf("label = %q", got[0].DisplayName)
		}
		if got[0].Description != "qwen3.5-35b-a3b" {
			t.Errorf("description = %q", got[0].Description)
		}
	})

	t.Run("a peer naming no model still gets a row", func(t *testing.T) {
		got := PeerDirectiveModels([]PeerFact{{DisplayID: "linux-gpu"}}, 5)
		if len(got) != 1 || got[0].DisplayName != "Waired peer: linux-gpu" {
			t.Errorf("got %+v", got)
		}
		if got[0].Description != "Another of your computers" {
			t.Errorf("description = %q, want the generic line rather than an empty one", got[0].Description)
		}
	})

	// Two machines whose names reduce to the same slug must stay two rows,
	// with two ids — otherwise the second is unreachable and the first is
	// ambiguous.
	//
	// Inverted (waired#1370 review): the ids used to be studio-mac,
	// studio-mac-2 and studio-mac-3. An ordinal names a position in the list,
	// and the resolver matched the slug and ignored it, so the "-2" row named
	// nothing and the first row named whichever machine came first. Each id
	// now ends in a hash of its own machine's key. The label ordinal now
	// counts identical names only: "studio mac" and "studio-mac" already
	// read differently, and a teammate's row would otherwise read
	// "studio-mac (佐藤) (2)".
	t.Run("colliding names each get a hashed id", func(t *testing.T) {
		got := PeerDirectiveModels([]PeerFact{
			{DisplayID: "studio mac", Key: "dev_1", Model: "a"},
			{DisplayID: "studio-mac", Key: "dev_2", Model: "b"},
			{DisplayID: "Studio.Mac", Key: "dev_3", Model: "c"},
		}, 5)
		if len(got) != 3 {
			t.Fatalf("got %d entries, want 3: %+v", len(got), got)
		}
		seen := map[string]bool{}
		for i, e := range got {
			if !regexp.MustCompile(`^waired/peer-studio-mac-[0-9a-f]{6}$`).MatchString(e.ID) {
				t.Errorf("entry %d id = %q, want studio-mac and a hash", i, e.ID)
			}
			if seen[e.ID] {
				t.Errorf("duplicate id %q — one of these rows is unreachable", e.ID)
			}
			seen[e.ID] = true
		}
		for _, e := range got {
			if strings.Contains(e.DisplayName, "(2)") || strings.Contains(e.DisplayName, "(3)") {
				t.Errorf("names that already read differently got an ordinal: %q", e.DisplayName)
			}
		}
	})

	t.Run("the same name twice gets an ordinal on the label", func(t *testing.T) {
		got := PeerDirectiveModels([]PeerFact{
			{DisplayID: "studio-mac", Key: "dev_1"},
			{DisplayID: "studio-mac", Key: "dev_2"},
		}, 5)
		if len(got) != 2 {
			t.Fatalf("got %d entries, want 2: %+v", len(got), got)
		}
		if got[0].DisplayName != "Waired peer: studio-mac" || got[1].DisplayName != "Waired peer: studio-mac (2)" {
			t.Errorf("labels = %q, %q", got[0].DisplayName, got[1].DisplayName)
		}
		if got[0].ID == got[1].ID {
			t.Errorf("two computers share id %q", got[0].ID)
		}
	})

	t.Run("an unnamable peer is skipped, not rendered blank", func(t *testing.T) {
		got := PeerDirectiveModels([]PeerFact{
			{DisplayID: "", Model: "a"},
			{DisplayID: "作業用", Model: "b"},
			{DisplayID: "linux-gpu", Model: "c"},
		}, 5)
		if len(got) != 1 || got[0].ID != "waired/peer-linux-gpu" {
			t.Errorf("got %+v, want only the nameable peer", got)
		}
	})

	t.Run("the cap bites, and counts rendered rows not input", func(t *testing.T) {
		in := []PeerFact{
			{DisplayID: ""},
			{DisplayID: "a"},
			{DisplayID: "b"},
			{DisplayID: "c"},
		}
		got := PeerDirectiveModels(in, 2)
		if len(got) != 2 {
			t.Fatalf("got %d entries, want 2: %+v", len(got), got)
		}
		if got[0].ID != "waired/peer-a" || got[1].ID != "waired/peer-b" {
			t.Errorf("the skipped peer consumed a slot: %+v", got)
		}
	})

	t.Run("zero and negative limits render nothing", func(t *testing.T) {
		for _, n := range []int{0, -1} {
			if got := PeerDirectiveModels([]PeerFact{{DisplayID: "a"}}, n); len(got) != 0 {
				t.Errorf("limit %d rendered %+v", n, got)
			}
		}
	})

	// A per-peer row must never collide with one of the fixed entries, or the
	// picker cache's dedupe would drop one of them.
	t.Run("never collides with a fixed directive id", func(t *testing.T) {
		fixed := map[string]bool{}
		for _, d := range DirectiveModels() {
			fixed[d.ID] = true
		}
		for _, d := range PeerDirectiveModels([]PeerFact{
			{DisplayID: "local"}, {DisplayID: "auto"}, {DisplayID: "cloud"}, {DisplayID: "peer"},
		}, 9) {
			if fixed[d.ID] {
				t.Errorf("per-peer id %q collides with a fixed directive", d.ID)
			}
		}
	})
}

// PIN: product contract — an id names one computer, and a turn carrying it is
// never served by a different one (waired-agent#325, the fail-closed pin;
// waired#1370 review for the cases below). The id shapes themselves are a
// record of today's behaviour.
func TestPeerDirectiveIDs(t *testing.T) {
	idsOf := func(peers ...PeerFact) []string { return PeerDirectiveIDs(peers) }
	hashed := regexp.MustCompile(`-[0-9a-f]{6}$`)

	t.Run("a common ASCII name keeps its readable id", func(t *testing.T) {
		got := idsOf(
			PeerFact{DisplayID: "linux-gpu", Key: "dev_a"},
			PeerFact{DisplayID: "mac-mini.local", Key: "dev_b"},
			PeerFact{DisplayID: "studio-mac (Alice Example)", Key: "dev_c"},
		)
		want := []string{"waired/peer-linux-gpu", "waired/peer-mac-mini-local", "waired/peer-studio-mac-alice-example"}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("id %d = %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("two teammates' computers with the same name and your own are three ids", func(t *testing.T) {
		got := idsOf(
			PeerFact{DisplayID: "studio-mac (田中)", Key: "dev_tanaka"},
			PeerFact{DisplayID: "studio-mac (佐藤)", Key: "dev_sato"},
			PeerFact{DisplayID: "studio-mac", Key: "dev_own", NotServing: true},
		)
		seen := map[string]bool{}
		for i, id := range got {
			if !hashed.MatchString(id) {
				t.Errorf("id %d = %q, want a hash: the slug alone is studio-mac for all three", i, id)
			}
			if seen[id] {
				t.Errorf("id %q names two computers", id)
			}
			seen[id] = true
		}
		// The hash follows the key, not the position.
		again := idsOf(
			PeerFact{DisplayID: "studio-mac", Key: "dev_own", NotServing: true},
			PeerFact{DisplayID: "studio-mac (佐藤)", Key: "dev_sato"},
			PeerFact{DisplayID: "studio-mac (田中)", Key: "dev_tanaka"},
		)
		if again[2] != got[0] || again[1] != got[1] || again[0] != got[2] {
			t.Errorf("reordering moved an id to another computer: %v then %v", got, again)
		}
	})

	t.Run("a non-ASCII name is hashed even with nothing to collide with", func(t *testing.T) {
		// Your own studio-mac may have been offered as waired/peer-studio-mac
		// yesterday. A teammate's studio-mac (田中) must not answer to it today.
		got := idsOf(PeerFact{DisplayID: "studio-mac (田中)", Key: "dev_tanaka"})
		if got[0] == "waired/peer-studio-mac" || !hashed.MatchString(got[0]) {
			t.Errorf("id = %q, want studio-mac and a hash", got[0])
		}
	})

	t.Run("a name the cap cuts is hashed", func(t *testing.T) {
		got := idsOf(
			PeerFact{DisplayID: "strix-halo-box (alice.example@example.com)", Key: "dev_1"},
			PeerFact{DisplayID: "strix-halo-box (alice.example@example.org)", Key: "dev_2"},
		)
		for i, id := range got {
			if !hashed.MatchString(id) {
				t.Errorf("id %d = %q, want a hash: both names cut to the same 32 bytes", i, id)
			}
			if slug := strings.TrimPrefix(id, PeerDirectivePrefix); len(slug) > peerSlugMaxBytes {
				t.Errorf("slug %q is %d bytes, over the %d cap", slug, len(slug), peerSlugMaxBytes)
			}
		}
		if got[0] == got[1] {
			t.Errorf("two computers share id %q", got[0])
		}
		// Alone, too: yesterday's list may have held the other one, and
		// its id must not come to name this computer today.
		if id := idsOf(PeerFact{DisplayID: "strix-halo-box (alice.example@example.com)", Key: "dev_1"})[0]; id != got[0] {
			t.Errorf("alone, id = %q; want %q, the same id it had beside its namesake", id, got[0])
		}
		// A name exactly at the cap loses nothing and needs no hash.
		exact := strings.Repeat("a", peerSlugMaxBytes)
		if id := idsOf(PeerFact{DisplayID: exact, Key: "dev_x"})[0]; id != PeerDirectivePrefix+exact {
			t.Errorf("id = %q, want the name unchanged", id)
		}
	})

	t.Run("an id that could be either computer names neither", func(t *testing.T) {
		got := idsOf(
			PeerFact{DisplayID: "studio-mac", Key: "dev_same"},
			PeerFact{DisplayID: "studio mac", Key: "dev_same"},
			PeerFact{DisplayID: "作業用 box"},
		)
		for i, id := range got {
			if id != "" {
				t.Errorf("id %d = %q, want none", i, id)
			}
		}
	})
}

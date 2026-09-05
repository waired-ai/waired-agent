package claudecode

import (
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
	t.Run("colliding names get an ordinal on both id and label", func(t *testing.T) {
		got := PeerDirectiveModels([]PeerFact{
			{DisplayID: "studio mac", Model: "a"},
			{DisplayID: "studio-mac", Model: "b"},
			{DisplayID: "Studio.Mac", Model: "c"},
		}, 5)
		want := []string{
			"waired/peer-studio-mac",
			"waired/peer-studio-mac-2",
			"waired/peer-studio-mac-3",
		}
		if len(got) != 3 {
			t.Fatalf("got %d entries, want 3: %+v", len(got), got)
		}
		seen := map[string]bool{}
		for i, w := range want {
			if got[i].ID != w {
				t.Errorf("entry %d id = %q, want %q", i, got[i].ID, w)
			}
			if seen[got[i].ID] {
				t.Errorf("duplicate id %q — one of these rows is unreachable", got[i].ID)
			}
			seen[got[i].ID] = true
		}
		if !strings.Contains(got[1].DisplayName, "(2)") {
			t.Errorf("the second row does not say which one it is: %q", got[1].DisplayName)
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

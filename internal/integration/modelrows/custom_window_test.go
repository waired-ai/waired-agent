package modelrows

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
)

// A computer serving a custom model under 200,704 tokens gets its per-computer
// row, carrying the window it serves (owner ruling 5 on waired-ai/waired#1473:
// no minimum window for custom models); a catalog model that declares nothing
// still gets none. Product contract: that ruling.
func TestFactsFromSnapshot_CustomModelUnderTheCodingWindow(t *testing.T) {
	small := peerView("small-box", "dev_s", "hf.co/acme/tiny-GGUF:Q4_K_M", true)
	small.InferenceState.ContextWindow = 0
	small.InferenceState.ActiveModel = "custom-tiny-0123abcd"
	small.InferenceState.CustomModelWindow = 40960
	bundled := peerView("old-box", "dev_o", "qwen3.5:4b", true)
	bundled.InferenceState.ContextWindow = 0
	bundled.InferenceState.CustomModelWindow = 40960 // not a custom model: ignored

	f := FactsFromSnapshot(&inferencemesh.Snapshot{Peers: []inferencemesh.PeerView{small, bundled}}, 5, false)
	if len(f.Peers) != 2 {
		t.Fatalf("peers = %+v", f.Peers)
	}
	if f.Peers[0].NotServing || f.Peers[0].ContextWindow != 40960 || f.Peers[0].Window1M {
		t.Errorf("the custom model's computer: %+v", f.Peers[0])
	}
	if !f.Peers[1].NotServing {
		t.Errorf("a catalog model declaring nothing got a row: %+v", f.Peers[1])
	}
}

// The rows naming one computer state its custom model's own window when it is
// below 200,704, so a client that sizes a conversation by the row compacts
// before the gateway refuses a turn; the rows that name no computer keep
// 200,704, since which computer answers them is decided per turn
// (waired-ai/waired#1481 item 5). Product contract: that issue.
func TestRows_StateACustomModelsOwnWindow(t *testing.T) {
	small := peerView("small-box", "dev_s", "hf.co/acme/tiny-GGUF:Q4_K_M", true)
	small.InferenceState.ContextWindow = 0
	small.InferenceState.ActiveModel = "custom-tiny-0123abcd"
	small.InferenceState.CustomModelWindow = 40960
	big := peerView("big-box", "dev_b", "qwen3.5:27b", true)
	big.InferenceState.ContextWindow = 262144 // a larger window is not stated
	self := peerView("this-box", "dev_me", "hf.co/acme/mid-GGUF:Q4_K_M", true)
	self.InferenceState.ContextWindow = 0
	self.InferenceState.ActiveModel = "custom-mid-89abcdef"
	self.InferenceState.CustomModelWindow = 131072

	rows := Rows(FactsFromSnapshot(&inferencemesh.Snapshot{Self: self, Peers: []inferencemesh.PeerView{small, big}}, 5, false))
	got := map[string]int{}
	for _, r := range rows {
		got[r.ID] = r.ContextWindow
	}
	for id, want := range map[string]int{
		"waired/local":          131072,
		"waired":                200704,
		"waired/peer":           200704,
		"waired/peer-small-box": 40960,
		"waired/peer-big-box":   200704,
	} {
		if got[id] != want {
			t.Errorf("%s states %d, want %d (rows %v)", id, got[id], want, got)
		}
	}
}

func TestRowWindow(t *testing.T) {
	for own, want := range map[int]int{0: 200704, 40960: 40960, 200704: 200704, 262144: 200704, -1: 200704} {
		if got := rowWindow(own); got != want {
			t.Errorf("rowWindow(%d) = %d, want %d", own, got, want)
		}
	}
}

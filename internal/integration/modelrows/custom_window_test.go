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

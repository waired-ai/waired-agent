package main

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
)

// Two axes, one word, four surfaces (waired-agent#837).
//
// `subsystem_state: "loading"` has always meant the model FILE is arriving
// from the network. Since waired-agent#879 the product also says "loaded" /
// "not loaded" about weights being in (V)RAM. Both reached the user as the
// same word — most visibly on the Claude status line, which rendered
// "local loading" for a download.
//
// PRODUCT CONTRACT (this change's decision record, and TRANSLATION.md row
// 158 for the memory sense, which is owner-approved and ja-pinned): the
// MEMORY axis keeps the word; the DISK axis gives it up. An allow-list is
// deliberately not used — the defect is a word nobody noticed, so this has
// to fail on anything new rather than on a wording change.

func TestResidencyRenderersNeverSayLoading(t *testing.T) {
	yes, no := true, false
	views := []residencyView{
		{},
		{InMemory: true, Model: "m:q4"},
		{InMemory: true, Model: "m:q4", Indefinite: true, IsActive: &yes},
		{InMemory: true, Model: "other:q4", Indefinite: true, IsActive: &no},
		{InMemory: true, Model: "m:q4", Until: "2026-08-20T13:11:43Z"},
		{StaleFor: 47 * time.Second},
	}
	for _, v := range views {
		got := residencyLine(v)
		if strings.Contains(strings.ToLower(got), "loading") {
			t.Errorf("residencyLine(%+v) = %q — the memory axis says loaded/not loaded, "+
				"never loading; that word is the disk axis", v, got)
		}
	}
	// The first-token row sits between the two axes and belongs to neither
	// (#912, landed in #953): it is a measurement, and a measurement is not
	// a state.
	now := time.Now()
	for _, ms := range []uint32{0, 420, 2600, 35400} {
		got := firstTokenLine(ms, now.Add(-time.Minute).Format(time.RFC3339), 259, now)
		if strings.Contains(strings.ToLower(got), "loading") {
			t.Errorf("firstTokenLine(%d) = %q — a measurement is not a state", ms, got)
		}
	}
}

func TestDownloadRenderersNeverSayLoaded(t *testing.T) {
	for _, st := range []string{"initializing", "starting", "loading", "awaiting_model", "degraded", "anything_else"} {
		got := prepMessage(management.InferenceStatus{SubsystemState: st})
		if strings.Contains(strings.ToLower(got), "loaded") {
			t.Errorf("prepMessage(%q) = %q — the disk axis must not borrow the memory axis's word", st, got)
		}
	}
}

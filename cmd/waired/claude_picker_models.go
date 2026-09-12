package main

import (
	"context"
	"fmt"
	"time"

	"github.com/waired-ai/waired-agent/internal/agentconfig"
	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/modelrows"
	"github.com/waired-ai/waired-agent/internal/management"
)

// What goes into this user's Waired /model rows: the fixed directive table,
// minus rows this computer cannot honour, plus one row per computer that is
// serving right now (waired-agent#830), plus a 1M twin wherever a side
// declares a 1M window (owner ruling 2026-09-06).
//
// Which rows those are is internal/integration/modelrows, shared with the
// daemon's GET /v1/models listing (waired-agent#1306) so the two surfaces
// cannot come to disagree about who is serving. What stays here is the
// rendering: the twins are a Claude Code mechanism — it sizes a session from a
// "[1m]" suffix in the id — and no other client has them.
//
// It runs in the unprivileged CLI child that owns the file, which has no
// daemon handle beyond the management API — so the mesh arrives over the same
// read route `waired peers list` uses, bounded and best-effort. Writing the
// rows must never turn a good `waired claude enable` into a failed one
// (claude_picker_write.go), and at enable time the daemon may legitimately
// have no network map yet, so "no peers" is the ordinary answer rather than a
// fault.

// pickerMeshTimeout bounds the mesh read. Matches `waired peers list`'s own
// budget; the sudo hop around this whole step allows 30s, and spending a
// meaningful slice of that on rows that are an enhancement would be the wrong
// trade.
const pickerMeshTimeout = 2 * time.Second

// pickerModels renders the rows from the facts.
//
// Each row is followed immediately by its 1M twin where one is offered, so
// the two spellings of one destination sit together rather than the twins
// collecting at the bottom of a menu that folds.
func pickerModels(f modelrows.Facts) []claudecode.PickerRow {
	rows := modelrows.Rows(f)
	out := make([]claudecode.PickerRow, 0, 2*len(rows))
	for _, r := range rows {
		out = append(out, claudecode.PickerRow{
			Model: r.ID, Label: r.DisplayName, Description: r.Description,
		})
		if !r.Window1M {
			continue
		}
		t := claudecode.Tier1MModel(r.DirectiveModel)
		out = append(out, claudecode.PickerRow{
			Model: t.ID, Label: t.DisplayName, Description: t.Description,
		})
	}
	return out
}

// pickerRows resolves the facts for real, reading the mesh over the
// management API. Degrades to the fixed table on any failure, with one warning
// line — the rows are an enhancement and the file must still be written.
func pickerRows(mgmtAddr string, peerLimit int) []claudecode.PickerRow {
	ctx, cancel := context.WithTimeout(context.Background(), pickerMeshTimeout)
	defer cancel()
	snap, err := fetchMeshSnapshotCtx(ctx, mgmtAddr)
	if err != nil {
		fmt.Fprintf(stderr, "Warning: couldn't read your network for the /model rows (%v). Writing the fixed rows only\n", err)
		snap = nil
	}
	return pickerModels(modelrows.FactsFromSnapshot(snap, peerLimit, publicShareEnabled(mgmtAddr)))
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

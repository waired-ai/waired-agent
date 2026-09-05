package claudecode

// The Waired rows of Claude Code's /model picker, published through the
// documented `modelPicker` setting (waired-agent#1185).
//
// They used to be written straight into Claude Code's own on-disk cache,
// ~/.claude/cache/gateway-models.json, because gateway model discovery is
// credential-gated: Claude Code queries a gateway's /v1/models only when it
// resolved an ANTHROPIC_AUTH_TOKEN, an apiKeyHelper value or an API key, and
// waired deliberately supplies none (waired-agent#488). Writing the private
// cache was the only way the rows appeared at all, at the price of a canary
// that re-measured a private file's shape every release
// (waired-agent#332 / #407 / #830).
//
// Since Claude Code v2.1.242 there is a documented way to say the same thing:
// `modelPicker` lists rows "in the order you write them and under labels you
// choose", each row's `model` "taken verbatim, so it accepts anything --model
// accepts"
// (https://code.claude.com/docs/en/settings-reference#modelpicker).
//
// Two things about it decide the shape of everything below.
//
// SCOPE. The key is read from managed settings, --settings and user settings,
// and "the highest of those three that sets the key supplies the whole
// lineup, and Claude Code never combines lineups from two sources". waired
// writes the USER file. That is the right tier twice over: it needs no
// elevation and can therefore follow the mesh at every session start, and an
// organisation lineup in managed settings outranks it whole rather than being
// silently merged into.
//
// OWNERSHIP. Because the winning source supplies the lineup whole, writing
// over someone else's user-scope lineup would delete it rather than add to
// it. So the lineup here is classified before it is touched, the same way
// modelsetting.go classifies a default model: absent, ours, or foreign — and
// foreign is left alone and reported.
//
// WHEN THE ROWS ARRIVE. Claude Code reads settings at startup and only then
// starts watching the file, so a write from a SessionStart hook lands before
// the watch is armed and is lost for that session (measured on 2.1.261,
// 2026-09-06: a synchronous hook write and writes at 1 s, 2 s and 3 s are all
// missed; writes at 6 s and 15 s are picked up — a race, not a contract). The
// rows a session sees are therefore the ones written before it started, and
// the hook is what makes that true of the NEXT session. Only the per-peer
// rows and the presence of the public row change with the mesh, so this
// costs one relaunch after a peer appears, and the docs say so.

import (
	"encoding/json"
	"fmt"
)

// modelPickerKey is Claude Code's settings key for the picker lineup.
const modelPickerKey = "modelPicker"

// PickerRow is one row of the lineup. The field names are Claude Code's.
//
// Description is a real second line: the picker renders it under the label,
// and a row without one reads "Custom model (<id>)" instead (measured on
// 2.1.261, 2026-09-06). The picker cache this replaces had no description
// field at all, which is why the per-peer rows used to fold the model name
// into the label.
type PickerRow struct {
	Model       string `json:"model"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
}

// pickerLineup is the value under modelPickerKey. replaceBuiltInOptions is
// deliberately absent from what waired writes — leaving it unset appends the
// Waired rows after Anthropic's built-in lineup, which is the whole point of
// picking a model here. It is decoded so that a foreign lineup that sets it
// is recognised as foreign rather than as a parse failure.
type pickerLineup struct {
	Options               []PickerRow `json:"options"`
	ReplaceBuiltInOptions *bool       `json:"replaceBuiltInOptions,omitempty"`
}

// PickerLineupKind says who owns the lineup in a settings file.
type PickerLineupKind int

const (
	// PickerLineupNone: no modelPicker key, or an empty lineup.
	PickerLineupNone PickerLineupKind = iota
	// PickerLineupOurs: every row names a Waired model id, so waired wrote
	// it and may rewrite or remove it.
	PickerLineupOurs
	// PickerLineupForeign: at least one row names something else. waired
	// never touches it — replacing it would delete the operator's own
	// lineup, since the winning source supplies the whole thing.
	PickerLineupForeign
	// PickerLineupUnreadable: the file is not JSON waired can parse, or the
	// key holds something that is not a lineup. Distinct from "absent": the
	// posture on a file it cannot read is hands off, not overwrite.
	PickerLineupUnreadable
)

// DetectPickerLineup classifies the lineup in a settings file and returns the
// rows it holds.
func DetectPickerLineup(path string) (PickerLineupKind, []PickerRow) {
	m, err := readSettings(path)
	if err != nil {
		return PickerLineupUnreadable, nil
	}
	raw, ok := m[modelPickerKey]
	if !ok {
		return PickerLineupNone, nil
	}
	var lineup pickerLineup
	if err := json.Unmarshal(raw, &lineup); err != nil {
		return PickerLineupUnreadable, nil
	}
	if len(lineup.Options) == 0 {
		return PickerLineupNone, nil
	}
	for _, row := range lineup.Options {
		if !IsWairedModelID(row.Model) {
			return PickerLineupForeign, lineup.Options
		}
	}
	return PickerLineupOurs, lineup.Options
}

// WritePickerLineup publishes rows into the user's settings file, replacing a
// lineup waired owns and refusing to disturb anyone else's.
//
// An empty row set removes the key instead of writing an empty lineup: Claude
// Code drops rows it cannot serve and falls back to the built-in lineup when
// none survives, so an empty lineup and no lineup mean the same thing to it,
// and leaving the key behind would make the next classification read "ours"
// for a file waired has nothing to say about.
//
// changed reports whether the file was rewritten, so the SessionStart hook can
// stay silent on the common no-op.
func WritePickerLineup(path string, rows []PickerRow) (changed bool, err error) {
	kind, current := DetectPickerLineup(path)
	switch kind {
	case PickerLineupForeign:
		return false, fmt.Errorf("claudecode: %s already lists its own /model rows; "+
			"waired left them alone", path)
	case PickerLineupUnreadable:
		return false, fmt.Errorf("claudecode: %s is not settings waired can read; "+
			"waired left it alone", path)
	}
	if pickerRowsEqual(current, rows) {
		return false, nil
	}
	m, err := readSettings(path)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		if _, ok := m[modelPickerKey]; !ok {
			return false, nil
		}
		delete(m, modelPickerKey)
		return true, writeSettings(path, m)
	}
	encoded, err := json.Marshal(pickerLineup{Options: rows})
	if err != nil {
		return false, fmt.Errorf("claudecode: encode %s: %w", modelPickerKey, err)
	}
	m[modelPickerKey] = encoded
	return true, writeSettings(path, m)
}

// RemovePickerLineup drops a lineup waired owns and reports whether it did.
// A foreign or unreadable lineup is left in place — `waired claude disable`
// undoes waired's own writes, not the operator's.
func RemovePickerLineup(path string) (removed bool, err error) {
	if kind, _ := DetectPickerLineup(path); kind != PickerLineupOurs {
		return false, nil
	}
	return WritePickerLineup(path, nil)
}

func pickerRowsEqual(a, b []PickerRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

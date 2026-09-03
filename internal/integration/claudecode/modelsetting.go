package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
)

// The Claude Code default model, in the user's own settings (waired-agent#1037).
//
// Since a model id now decides where a turn runs, the id a session starts on
// decides where it runs by default — and Claude Code's own default is a real
// Anthropic model, which would send every untouched session to the real API and
// leave this computer's hardware idle. Writing a Waired id as the default is
// what keeps "install waired, open Claude Code, your own model answers" true
// after the change, and it makes the footer and the /model row agree with what
// actually happens instead of naming a model that is not answering.
//
// It goes in ~/.claude/settings.json — the same file Claude Code writes when a
// person picks a model and confirms "set as default", and the same file this
// package's statusLine already owns a key in. Managed settings can hold a model
// too, and must not be used for this: Claude Code re-applies a managed model at
// every start and says so ("Managed settings pins … that applies on restart"),
// so an operator who picked something else would find it undone every session.
// This is a default, not a pin.
//
// Ownership follows the statusLine's rules, for the statusLine's reasons:
//
//   - absent → write it.
//   - a Waired id → ours, refresh or remove it.
//   - anything else → the operator's choice, and choices are not ours to
//     overwrite. `waired claude enable` says so rather than acting.
const modelSettingKey = "model"

// ModelSettingKind classifies what the settings file says about the default
// model, from waired's point of view.
type ModelSettingKind string

const (
	// ModelSettingNone: no default recorded. Claude Code will use its own.
	ModelSettingNone ModelSettingKind = "none"
	// ModelSettingOurs: a Waired id — either waired wrote it, or the operator
	// picked a Waired row and made it their default. Both are ours to manage:
	// the value is the same either way, and a pick that agrees with what we
	// would have written is not a conflict.
	ModelSettingOurs ModelSettingKind = "ours"
	// ModelSettingForeign: a model id that is not Waired's. Left alone.
	ModelSettingForeign ModelSettingKind = "foreign"
)

// DetectModelSetting classifies the default model recorded in the user's
// settings without changing anything.
func DetectModelSetting(home string) (ModelSettingKind, string, error) {
	m, err := readSettings(SettingsPath(home))
	if err != nil {
		return ModelSettingNone, "", err
	}
	return classifyModelSetting(m)
}

func classifyModelSetting(m map[string]json.RawMessage) (ModelSettingKind, string, error) {
	raw, ok := m[modelSettingKey]
	if !ok {
		return ModelSettingNone, "", nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil || id == "" {
		// Present and not a model id we can reason about. Foreign, so it is
		// left exactly as found — the same posture classifyStatusLine takes.
		return ModelSettingForeign, "", nil
	}
	if IsWairedModelID(id) {
		return ModelSettingOurs, id, nil
	}
	return ModelSettingForeign, id, nil
}

// RemoveModelSetting drops the default model when it is a Waired id, and
// leaves anything else alone. The file goes with it if that was the last key —
// the same tidiness RemoveStatusLine keeps, and the reason both have to know
// about it: whichever of the two runs last is the one that finds the file
// empty.
func RemoveModelSetting(home string) error {
	path := SettingsPath(home)
	m, err := readSettings(path)
	if err != nil {
		return err
	}
	kind, _, err := classifyModelSetting(m)
	if err != nil || kind != ModelSettingOurs {
		return err
	}
	delete(m, modelSettingKey)
	if len(m) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("claudecode: remove %s: %w", path, err)
		}
		return nil
	}
	return writeSettings(path, m)
}

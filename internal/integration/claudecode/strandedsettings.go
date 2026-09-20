package claudecode

// The settings file waired is NOT writing (waired-agent#1457).
//
// CLAUDE_CONFIG_DIR relocates the whole config directory, so which file
// SettingsPath names depends on an environment variable a person can set and
// unset between two runs. Whichever one waired wrote first keeps its keys, and
// they come back the moment the variable goes the other way: rows offering
// computers the gateway no longer routes to, a status line running a wrapper
// script `waired claude disable` deleted.
//
// Nothing here writes new content. It removes waired's own keys from the twin,
// under the same ownership rules the live path uses — a lineup, a status line
// or a default model somebody else set is theirs in both files.

import (
	"errors"
	"fmt"
	"os"
)

// RemoveWairedSettingsAt takes waired's four keys out of one settings file and
// reports whether it removed any: the /model lineup, the status line, the
// default model, and the subagent placement. The file goes with them when they
// were the last keys in it.
//
// Best effort per key: one key waired must not touch does not stop the others,
// and an error from one is returned only after the rest have been tried.
func RemoveWairedSettingsAt(path string) (removed bool, err error) {
	var errs []error

	if gone, e := RemovePickerLineup(path); e != nil {
		errs = append(errs, e)
	} else if gone {
		removed = true
	}

	if gone, e := removeStatusLineAt(path); e != nil {
		errs = append(errs, e)
	} else if gone {
		removed = true
	}

	if gone, e := removeModelSettingAt(path); e != nil {
		errs = append(errs, e)
	} else if gone {
		removed = true
	}

	// SubagentForeign and SubagentUnreadable both come back as errors, and
	// both mean "not ours" rather than "broken", so they are dropped here the
	// way the other three drop a foreign value.
	if gone, e := SetSubagentPlacement(path, SubagentFollow, ""); e == nil && gone {
		removed = true
	}

	if removed {
		if e := removeIfEmpty(path); e != nil {
			errs = append(errs, e)
		}
	}
	return removed, errors.Join(errs...)
}

// removeStatusLineAt is RemoveStatusLine's map half, without the wrapper
// artifacts: those live under the home directory rather than the config
// directory, so the live path's removal already reaches them and the twin has
// none of its own.
func removeStatusLineAt(path string) (removed bool, err error) {
	m, err := readSettings(path)
	if err != nil {
		return false, err
	}
	switch kind, _ := classifyStatusLine(m); kind {
	case StatusLineNone, StatusLineForeign:
		return false, nil
	case StatusLineOurs:
		delete(m, statuslineKey)
		delete(m, statuslineStashKey)
	case StatusLineWrapped:
		if stash, ok := m[statuslineStashKey]; ok {
			m[statuslineKey] = stash
		} else {
			delete(m, statuslineKey)
		}
		delete(m, statuslineStashKey)
	}
	return true, writeSettings(path, m)
}

// removeModelSettingAt is RemoveModelSetting against a path.
func removeModelSettingAt(path string) (removed bool, err error) {
	m, err := readSettings(path)
	if err != nil {
		return false, err
	}
	kind, _, err := classifyModelSetting(m)
	if err != nil || kind != ModelSettingOurs {
		return false, err
	}
	delete(m, modelSettingKey)
	return true, writeSettings(path, m)
}

// removeIfEmpty deletes a settings file waired has just emptied. The same
// tidiness RemoveStatusLine and RemoveModelSetting keep, in one place because
// here four removals share the question of who was last.
func removeIfEmpty(path string) error {
	m, err := readSettings(path)
	if err != nil || len(m) != 0 {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("claudecode: remove %s: %w", path, err)
	}
	return nil
}

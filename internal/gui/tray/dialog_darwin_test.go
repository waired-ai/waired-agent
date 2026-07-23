//go:build darwin

package tray

import (
	"strings"
	"testing"
)

// TestConfirmWithLabels_DefaultsToCancel: the consent dialog is the only
// gate between "nothing shared" and "strangers may use this machine",
// and it takes focus when it appears — so Return must dismiss it, not
// accept it (waired#901 L5). ConfirmYesNo makes the same choice and says
// so; this helper used to contradict it.
func TestConfirmWithLabels_DefaultsToCancel(t *testing.T) {
	const accept = "OK — share my machine"
	const cancel = "Not now"
	script := osascriptDialogScript("Share this computer?", "Body", "caution",
		[]string{cancel, accept}, cancel)

	if !strings.Contains(script, `default button "`+cancel+`"`) {
		t.Errorf("consent dialog must default to the cancel button; script = %s", script)
	}
	if strings.Contains(script, `default button "`+accept+`"`) {
		t.Errorf("consent dialog must not default to accept; script = %s", script)
	}
	// The affirmative button still renders second, matching ConfirmYesNo's
	// layout — only the default key binding moved.
	if i, j := strings.Index(script, cancel), strings.LastIndex(script, accept); i < 0 || j < 0 || i > j {
		t.Errorf("button order should stay {cancel, accept}; script = %s", script)
	}
}

// TestOsascriptDialogScript_OmitsDefaultWhenUnset keeps the builder
// honest for the callers that pass no default button.
func TestOsascriptDialogScript_OmitsDefaultWhenUnset(t *testing.T) {
	script := osascriptDialogScript("T", "B", "", []string{"A", "B"}, "")
	if strings.Contains(script, "default button") {
		t.Errorf("no default button was requested; script = %s", script)
	}
}

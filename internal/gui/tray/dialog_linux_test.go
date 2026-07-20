//go:build linux

package tray

import (
	"strings"
	"testing"
)

// TestConfirmCandidates_OrderAndShape pins the dialog spawn list: zenity
// before kdialog (broader install base), each program getting the
// expected --question / --yesno argument plus the title and body
// passed verbatim. A future package change that flips the preference
// order or drops one of the binaries would surface here.
func TestConfirmCandidates_OrderAndShape(t *testing.T) {
	got := confirmCandidates("Reconfigure?", "Body of the prompt")
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}
	if got[0].binary != "zenity" {
		t.Errorf("first candidate = %q, want zenity", got[0].binary)
	}
	if got[1].binary != "kdialog" {
		t.Errorf("second candidate = %q, want kdialog", got[1].binary)
	}

	if !sliceContainsExact(got[0].args, "--question") {
		t.Errorf("zenity args missing --question: %v", got[0].args)
	}
	if !sliceContainsAny(got[0].args, "--title=Reconfigure?") {
		t.Errorf("zenity args missing --title=Reconfigure?: %v", got[0].args)
	}
	if !sliceContainsAny(got[0].args, "--text=Body of the prompt") {
		t.Errorf("zenity args missing --text=...: %v", got[0].args)
	}

	if !sliceContainsExact(got[1].args, "--yesno") {
		t.Errorf("kdialog args missing --yesno: %v", got[1].args)
	}
	if !sliceContainsExact(got[1].args, "Body of the prompt") {
		t.Errorf("kdialog args missing body text: %v", got[1].args)
	}
	// kdialog uses `--title <title>` (separate args), not `--title=<title>`.
	for i, a := range got[1].args {
		if a == "--title" && i+1 < len(got[1].args) && got[1].args[i+1] == "Reconfigure?" {
			return
		}
	}
	t.Errorf("kdialog args missing `--title Reconfigure?` pair: %v", got[1].args)
}

// TestConfirmYesNo_NoBackendsFalseFalse verifies the contract callers
// rely on: when no dialog binaries are available, ConfirmYesNo must
// return (false, false) so the caller knows to fall back to a
// non-interactive route (clipboard + notification). Achieved here by
// stubbing $PATH with a directory containing no `zenity` / `kdialog`
// executables.
func TestConfirmYesNo_NoBackendsFalseFalse(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	yes, ok := ConfirmYesNo("Title", "Body")
	if yes || ok {
		t.Errorf("got yes=%v ok=%v, want false/false when no backends present", yes, ok)
	}
}

// TestConfirmLabelCandidates_CarryLabels pins that the caller-supplied
// accept/cancel labels reach each backend verbatim: zenity via
// --ok-label/--cancel-label, kdialog via --yes-label/--no-label (which
// must precede --yesno). This is what lets the server-authored consent
// wording (waired#833) render identically across desktops.
func TestConfirmLabelCandidates_CarryLabels(t *testing.T) {
	accept := "OK — share my machine and start"
	cancel := "Not now"
	got := confirmLabelCandidates("Use public nodes?", "Body", accept, cancel)
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}
	if got[0].binary != "zenity" || got[1].binary != "kdialog" {
		t.Fatalf("candidate order = %q,%q want zenity,kdialog", got[0].binary, got[1].binary)
	}

	if !sliceContainsExact(got[0].args, "--ok-label="+accept) {
		t.Errorf("zenity args missing --ok-label=%q: %v", accept, got[0].args)
	}
	if !sliceContainsExact(got[0].args, "--cancel-label="+cancel) {
		t.Errorf("zenity args missing --cancel-label=%q: %v", cancel, got[0].args)
	}

	if !sliceContainsExact(got[1].args, accept) {
		t.Errorf("kdialog args missing accept label %q: %v", accept, got[1].args)
	}
	if !sliceContainsExact(got[1].args, cancel) {
		t.Errorf("kdialog args missing cancel label %q: %v", cancel, got[1].args)
	}
	// --yes-label / --no-label must come before --yesno for kdialog.
	var yesLabelIdx, yesnoIdx = -1, -1
	for i, a := range got[1].args {
		switch a {
		case "--yes-label":
			yesLabelIdx = i
		case "--yesno":
			yesnoIdx = i
		}
	}
	if yesLabelIdx < 0 || yesnoIdx < 0 || yesLabelIdx > yesnoIdx {
		t.Errorf("kdialog --yes-label must precede --yesno: %v", got[1].args)
	}
}

// TestConfirmWithLabels_NoBackendsFalseFalse mirrors the ConfirmYesNo
// contract: no dialog binary on PATH → (false,false) so callers fall
// back to a non-interactive route.
func TestConfirmWithLabels_NoBackendsFalseFalse(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	confirmed, ok := ConfirmWithLabels("Title", "Body", "Accept", "Cancel")
	if confirmed || ok {
		t.Errorf("got confirmed=%v ok=%v, want false/false when no backends present", confirmed, ok)
	}
}

func sliceContainsExact(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func sliceContainsAny(s []string, prefix string) bool {
	for _, v := range s {
		if v == prefix || strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

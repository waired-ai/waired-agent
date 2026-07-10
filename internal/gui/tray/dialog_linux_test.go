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

package tray

import (
	"strings"
	"testing"
)

// One elevation prompt at a time. Menu clicks dispatch on their own goroutine
// (the consent UI blocks), so nothing else stops a double-click from stacking
// two UAC dialogs / polkit agents.
func TestClaimStartAgent_OnlyOneAtATime(t *testing.T) {
	tr := &tray{last: MenuModel{StartAgentAction: startAgentActionLabel}}

	if !tr.claimStartAgent() {
		t.Fatal("first click did not claim the start slot")
	}
	if tr.claimStartAgent() {
		t.Error("second click claimed the slot while the first was still in flight")
	}

	tr.releaseStartAgent()
	if !tr.claimStartAgent() {
		t.Error("slot was not released after the first start finished")
	}
}

// The click handler runs on its own goroutine, so the menu may have moved on
// by the time it is scheduled — the daemon may have come up on its own. The
// handler re-reads the latched model rather than trusting that the row it was
// attached to is still being offered.
func TestClaimStartAgent_DeclinesWhenTheActionIsNotOffered(t *testing.T) {
	tr := &tray{last: MenuModel{}}
	if tr.claimStartAgent() {
		t.Error("claimed the start slot with no start action in the model")
	}
}

func TestOnCopyStartCommand_CopiesTheCommand(t *testing.T) {
	l := resetSeams(t)
	tr := &tray{last: MenuModel{
		StartAgentCopy: startAgentCopyLabel,
		StartAgentCmd:  "sudo systemctl start waired-agent",
	}}

	tr.onCopyStartCommand()

	got := l.snapshot(&l.clipboard)
	if len(got) != 1 || got[0] != "sudo systemctl start waired-agent" {
		t.Errorf("clipboard = %v, want the start command", got)
	}
}

func TestOnCopyStartCommand_NoCommandIsANoOp(t *testing.T) {
	l := resetSeams(t)
	tr := &tray{last: MenuModel{}}

	tr.onCopyStartCommand()

	if got := l.snapshot(&l.clipboard); len(got) != 0 {
		t.Errorf("clipboard = %v, want nothing copied", got)
	}
}

// When the start fails, hand the user the command instead of a dead end. This
// matters most for the failure that has no consent UI at all — a Linux session
// with no polkit agent — where a modal error dialog may be just as unavailable
// as the prompt was, which is why the clipboard route is preferred over
// showError.
func TestOfferStartCommand_PrefersTheClipboardOverAModal(t *testing.T) {
	l := resetSeams(t)
	tr := &tray{last: MenuModel{StartAgentCmd: "Start-Service waired-agent"}}

	tr.offerStartCommand("Could not start the Waired agent: UAC consent declined")

	if got := l.snapshot(&l.clipboard); len(got) != 1 || got[0] != "Start-Service waired-agent" {
		t.Errorf("clipboard = %v, want the start command", got)
	}
	if got := l.snapshot(&l.errors); len(got) != 0 {
		t.Errorf("showError called as well: %v", got)
	}
}

// With no command to offer there is nothing to copy, so say it plainly.
func TestOfferStartCommand_FallsBackToTheErrorDialog(t *testing.T) {
	l := resetSeams(t)
	tr := &tray{last: MenuModel{}}

	tr.offerStartCommand("Could not start the Waired agent: boom")

	got := l.snapshot(&l.errors)
	if len(got) != 1 || !strings.Contains(got[0], "boom") {
		t.Errorf("errors = %v, want the failure reported", got)
	}
}

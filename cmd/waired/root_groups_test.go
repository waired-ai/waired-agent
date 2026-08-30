package main

import "testing"

// Every command on the root belongs to one of the help groups.
//
// cobra renders a command with no GroupID in a trailing "Additional
// Commands" block, detached from the five headings the help is built
// around. `waired share` landed there for a release: it was added to the
// root and not to groupFor, and nothing said so (waired#1305). The help
// text is a surface like any other, and this is the assertion that keeps
// a new command from arriving outside it.
func TestEveryRootCommandHasAHelpGroup(t *testing.T) {
	root := newRootCmd()
	groups := map[string]bool{}
	for _, g := range root.Groups() {
		groups[g.ID] = true
	}
	if len(groups) == 0 {
		t.Fatal("the root declares no help groups")
	}

	seen := 0
	for _, c := range root.Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			// Placed by SetHelpCommandGroupID / SetCompletionCommandGroupID
			// after cobra creates them, which is later than this.
			continue
		}
		if c.Hidden {
			// A hidden command renders nowhere, so it has no group to be
			// in — `waired proxy` is a redirect kept for people who type
			// the retired name.
			continue
		}
		seen++
		if c.GroupID == "" {
			t.Errorf("`waired %s` has no help group, so it renders in the ungrouped "+
				"trailing block — add it to groupFor", c.Name())
			continue
		}
		if !groups[c.GroupID] {
			t.Errorf("`waired %s` is in group %q, which the root does not declare", c.Name(), c.GroupID)
		}
	}
	if seen < 10 {
		t.Fatalf("walked %d root commands, want at least 10 — did newRootCmd change shape?", seen)
	}
}

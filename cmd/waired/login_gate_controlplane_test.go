package main

import (
	"strings"
	"testing"
)

// The sign-in prompt names the control plane it is about to enrol into.
//
// PRODUCT CONTRACT — waired-agent#800. The host in that report lost its
// state dir, agent.env went with it, and `waired init` printed a
// PRODUCTION link on a machine that had been enrolled against dev. The
// host was in the link all along, buried at the end of a URL nobody reads
// that far, and nothing on screen said the target had changed.
//
// Same label as `waired status` (cmd/waired/main.go) on purpose: one name
// for one thing. "Control Plane" is the docs glossary's own headword,
// cross-linked to "coordination service", so it is the established term
// rather than a new one.
func TestPresentLoginURL_NamesTheControlPlane(t *testing.T) {
	stubOpener(t, nil)
	var out strings.Builder
	presentLoginURL(nil, &out, "https://app.waired.ai/login/abc", "", "https://app.waired.ai", gatePrintOnly)

	if !strings.Contains(out.String(), "Control Plane: https://app.waired.ai") {
		t.Errorf("sign-in prompt does not name the control plane: %q", out.String())
	}
}

// A caller with no control URL to show must not print an empty label.
//
// Record of today's behaviour: every production caller passes one, so this
// only keeps a dangling label out of the output if one ever does not.
func TestPresentLoginURL_OmitsAnEmptyControlPlane(t *testing.T) {
	stubOpener(t, nil)
	var out strings.Builder
	presentLoginURL(nil, &out, "https://cp.example/login/abc", "", "", gatePrintOnly)

	if strings.Contains(out.String(), "Control Plane:") {
		t.Errorf("printed an empty control-plane label: %q", out.String())
	}
}

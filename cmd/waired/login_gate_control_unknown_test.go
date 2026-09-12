package main

import (
	"strings"
	"testing"
)

// PRODUCT CONTRACT (waired-agent#1300): when this process could not read
// the file that records which Control Plane this computer uses, the
// sign-in prompt says so instead of naming the built-in default.
//
// The unelevated run is the ordinary case, not an edge one: agent.env is
// owner-only, so a `waired init` without sudo resolves to the built-in
// production URL and printed it as a fact — directly under a sign-in link
// pointing at a different Control Plane, on a host enrolled against that
// one. Same shape as waired-agent#800, one layer along: there the link
// and the label disagreed because the file was gone, here because this
// process cannot see it.
func TestPresentLoginURL_SaysWhenItCouldNotReadTheControlPlane(t *testing.T) {
	stubOpener(t, nil)
	var out strings.Builder
	presentLoginURL(nil, &out, "https://cp.example/login/abc", "", "https://app.waired.ai", true, gatePrintOnly)

	got := out.String()
	if strings.Contains(got, "Control Plane: https://app.waired.ai") {
		t.Errorf("printed a guess as a fact: %q", got)
	}
	if !strings.Contains(got, "couldn't read this computer's setting from here") {
		t.Errorf("did not say why the Control Plane is not named: %q", got)
	}
	// The link is still the thing that counts, and the line says so —
	// the sign-in itself is the daemon's and it reads the file as root.
	if !strings.Contains(got, "https://cp.example/login/abc") {
		t.Errorf("the sign-in link must still be printed: %q", got)
	}
}

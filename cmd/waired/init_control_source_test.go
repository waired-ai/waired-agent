package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/controlurl"
)

// PRODUCT CONTRACT — waired-agent#800. Losing the state dir loses
// agent.env with it (it lives inside the state dir on macOS and Windows),
// so controlurl.Resolve falls through to the production default. The
// device is still enrolled to whatever it was enrolled to, and nobody
// asked to change that.
//
// Observed on sv-macmini 2026-08-15 against a build with #803: `waired
// init` after the wipe failed with
//
//	already enrolled to https://app.dev.waired.net — run `waired logout`
//	first to switch control planes (requested https://app.waired.ai)
//
// and never reached the daemon, so the repair on the resume path could not
// run. The advice is wrong for the situation too: the operator did not
// switch control planes, they lost the file that recorded one.
//
// Only the built-in default defers. An explicit --control or
// $WAIRED_CONTROL_URL is a request, and a request to move a device to
// another control plane must still be refused.
func TestControlForRenew(t *testing.T) {
	const enrolled = "https://app.dev.waired.net"
	for _, tc := range []struct {
		name     string
		resolved string
		src      controlurl.Source
		enrolled string
		want     string
	}{
		{
			name:     "the #800 shape: nothing configured, device enrolled elsewhere",
			resolved: controlurl.Default, src: controlurl.SourceBuiltin, enrolled: enrolled,
			want: enrolled,
		},
		{
			name:     "explicit --control still means what it says",
			resolved: "https://app.waired.ai", src: controlurl.SourceOperator, enrolled: enrolled,
			want: "https://app.waired.ai",
		},
		{
			name:     "agent.env still means what it says",
			resolved: "https://app.waired.ai", src: controlurl.SourceInstaller, enrolled: enrolled,
			want: "https://app.waired.ai",
		},
		{
			name:     "built-in default on a device with no enrollment",
			resolved: controlurl.Default, src: controlurl.SourceBuiltin, enrolled: "",
			want: controlurl.Default,
		},
		{
			name:     "built-in default that already matches",
			resolved: controlurl.Default, src: controlurl.SourceBuiltin, enrolled: controlurl.Default,
			want: controlurl.Default,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := controlForRenew(tc.resolved, tc.src, tc.enrolled); got != tc.want {
				t.Fatalf("controlForRenew(%q, %q, %q) = %q, want %q",
					tc.resolved, tc.src, tc.enrolled, got, tc.want)
			}
		})
	}
}

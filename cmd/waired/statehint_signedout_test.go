package main

import (
	"strings"
	"testing"
)

// The system state dir is 0700 root (SYSTEM+Administrators DACL'd on Windows)
// whether or not anything is enrolled inside it, so an unelevated caller gets
// fs.ErrPermission either way. Reading that error as "enrolled" is how a
// computer that had just been signed out answered `waired status` with "This
// computer is signed in system-wide" — measured on macOS against 0.0.3-rc6,
// in the same minute `sudo waired status` said "Not signed in."
// (waired-agent#1272, the narrower twin of waired-agent#1005 and #1269).
//
// PIN: product contract — a permission error is not evidence of an enrollment;
// the answer comes from something that observed one
// (waired-ai/waired-agent#1272).
func TestSystemStateNoticeDoesNotInventAnEnrollment(t *testing.T) {
	const sys = "/Library/Application Support/waired"

	t.Run("the service says signed in", func(t *testing.T) {
		got, ok := systemStateNotice(sys, "waired status", "darwin", enrolmentSignedIn)
		if !ok {
			t.Fatal("want a notice when the enrollment was actually observed")
		}
		if !strings.Contains(got, "signed in system-wide") {
			t.Errorf("notice = %q, want the enrolled wording", got)
		}
		if !strings.Contains(got, "sudo waired status") {
			t.Errorf("notice = %q, want the elevation hint", got)
		}
	})

	t.Run("the service says signed out", func(t *testing.T) {
		got, ok := systemStateNotice(sys, "waired status", "darwin", enrolmentSignedOut)
		if ok {
			t.Errorf("a signed-out computer got a notice claiming otherwise: %q", got)
		}
	})

	t.Run("the service did not answer", func(t *testing.T) {
		got, ok := systemStateNotice(sys, "waired status", "darwin", enrolmentUnknown)
		if !ok {
			t.Fatal("want something to print when nothing could be observed")
		}
		// The assertion is on the CLAIM, not on the words: the honest
		// wording ends in "whether the computer is signed in", so matching
		// that substring alone would fail on the fix and pass on the defect.
		if strings.Contains(got, "This computer is signed in system-wide") {
			t.Errorf("notice = %q, still asserts an enrollment nothing observed", got)
		}
		if !strings.Contains(got, "can't tell") {
			t.Errorf("notice = %q, does not say that it cannot tell", got)
		}
		if !strings.Contains(got, "sudo waired status") {
			t.Errorf("notice = %q, want the elevation hint", got)
		}
	})
}

// The fallback path drops through to the caller's plain "Not signed in"
// message when the service says so, rather than printing a notice about a
// directory the caller cannot read.
func TestResolveSystemFallbackSignedOutFallsThrough(t *testing.T) {
	dir, id, notice := resolveSystemFallbackAt(t.TempDir(), t.TempDir(), "waired status", "linux", enrolmentSignedOut)
	if dir != "" || id != nil || notice != "" {
		t.Errorf("want the plain not-signed-in path; got dir=%q id=%v notice=%q", dir, id, notice)
	}
}

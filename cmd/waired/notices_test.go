package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration"
	"github.com/waired-ai/waired-agent/internal/management"
	notices "github.com/waired-ai/waired-agent/internal/notice"
)

// noticeDaemon is a fake daemon serving /waired/v1/notices. A nil
// handler answers 404, which is how "a daemon older than the route" is
// expressed.
func noticeDaemon(t *testing.T, ns []notices.Notice, found bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/notices" || !found {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(management.NoticesResponse{Notices: ns})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func lighterNotice() notices.Notice {
	return notices.LighterModel("qwen3-30b-a3b", "qwen3-8b-instruct", 42, 60)
}

// TestSanitiseStripsEveryMarkThisCLIFolds
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05: the notice module refuses
// what a renderer would misread). A notice carrying ✓ or ✗ would forge a
// doctor status mark, and a notice carrying ● would forge a state dot.
//
// The list lives in ascii.go, in package main, so internal/notice cannot
// import it. This test is where the two meet: it fails the moment the
// fold table grows a mark the sanitiser does not know, which a second
// copy of the list over there would not.
func TestSanitiseStripsEveryMarkThisCLIFolds(t *testing.T) {
	for _, glyph := range markerGlyphs() {
		if got := notices.Sanitise("before" + glyph + "after"); strings.Contains(got, glyph) {
			t.Errorf("notice.Sanitise kept %q (fold table entry): %q", glyph, got)
		}
	}
}

// TestFetchNotices_OlderDaemonIsNotAnError
//
// PRODUCT CONTRACT: the 404-means-the-daemon-predates-this convention
// (internal/management/socket.go, and every best-effort read in this
// CLI). Both callers show nothing for an older daemon and nothing for a
// healthy one, so the two must not be told apart here.
func TestFetchNotices_OlderDaemonIsNotAnError(t *testing.T) {
	got, err := fetchNotices(noticeDaemon(t, nil, false))
	if err != nil {
		t.Fatalf("404 became an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want none", got)
	}
}

func TestFetchNotices_ReadsWhatTheDaemonPublishes(t *testing.T) {
	got, err := fetchNotices(noticeDaemon(t, []notices.Notice{lighterNotice()}, true))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 1 || got[0].Target != "qwen3-8b-instruct" {
		t.Fatalf("got %+v", got)
	}
}

// TestFetchNotices_SanitisesOnTheWayIn
//
// PRODUCT CONTRACT (owner ruling above). This CLI prints to a terminal,
// which EXECUTES an escape sequence rather than showing it, so the
// decode side of the invariant is the one that matters here.
func TestFetchNotices_SanitisesOnTheWayIn(t *testing.T) {
	hostile := notices.Notice{
		Kind: "x", Severity: notices.SeverityWarn,
		Title: "clean\x1b[2J✓ all good", Text: "two\nlines",
	}
	got, err := fetchNotices(noticeDaemon(t, []notices.Notice{hostile}, true))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	if strings.ContainsAny(got[0].Title+got[0].Text, "\n\r\x1b") || strings.Contains(got[0].Title, "✓") {
		t.Fatalf("hostile notice survived the decode: %+v", got[0])
	}
}

func TestPrintNotices_RendersTheBlock(t *testing.T) {
	url := noticeDaemon(t, []notices.Notice{lighterNotice()}, true)

	out := captureStdout(t, func() { printNotices(url) })

	for _, want := range []string{
		"Notices:",
		"Lighter model recommended — switch to qwen3-8b-instruct",
		"This computer answers at 42 tok/s with qwen3-30b-a3b, below the 60 tok/s floor.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

// TestPrintNotices_OmitsAnEmptyBlock
//
// PRODUCT CONTRACT: this CLI elides a section it has nothing to put in
// (status_observability.go says so for the block beside this one). On a
// healthy computer there is usually nothing to say, and a standing
// heading with no rows is a section people learn to skip.
func TestPrintNotices_OmitsAnEmptyBlock(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"nothing published", noticeDaemon(t, nil, true)},
		{"older daemon", noticeDaemon(t, nil, false)},
		{"daemon down", "http://127.0.0.1:65535"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if out := captureStdout(t, func() { printNotices(tc.url) }); strings.TrimSpace(out) != "" {
				t.Errorf("printed %q, want nothing", out)
			}
		})
	}
}

// TestNoticeFindings_BecomeWarnRows records today's behaviour: a notice
// is one doctor line, in the shape every other finding uses.
func TestNoticeFindings_BecomeWarnRows(t *testing.T) {
	got := noticeFindings(noticeDaemon(t, []notices.Notice{lighterNotice()}, true))

	if len(got) != 1 {
		t.Fatalf("got %+v, want one finding", got)
	}
	if got[0].Status != integration.StatusWarn {
		t.Errorf("status = %v, want warn", got[0].Status)
	}
	// The subject column carries a short noun, like every other doctor
	// row; the sentence goes in the detail, with the figures behind it.
	if got[0].Subject != "model suggestion" {
		t.Errorf("subject = %q, want a short noun", got[0].Subject)
	}
	rendered := formatFinding(got[0])
	for _, want := range []string{
		"⚠ model suggestion — Lighter model recommended",
		"switch to qwen3-8b-instruct.",
		"below the 60 tok/s floor.",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered = %q, missing %q", rendered, want)
		}
	}
	if strings.ContainsAny(rendered, "\n") {
		t.Errorf("a doctor finding is one line: %q", rendered)
	}
}

// TestNoticeSubject_FallsBackForAnUnknownNotice records today's
// behaviour: a notice from a newer daemon that this build does not
// recognise still renders, rather than leaving the subject column blank.
func TestNoticeSubject_FallsBackForAnUnknownNotice(t *testing.T) {
	if got := noticeSubject(notices.Notice{Kind: "something_new"}); got != "something_new" {
		t.Errorf("got %q, want the kind", got)
	}
	if got := noticeSubject(notices.Notice{}); got != "notice" {
		t.Errorf("got %q, want a last-resort subject", got)
	}
}

// TestNoticeFindings_NeverMoveTheExitCode
//
// PRODUCT CONTRACT (doctor.go's countFails: only StatusFail sets the
// exit code, and doctor_observability.go states the same rule for the
// findings beside these). Advice must not make a script think the setup
// is broken.
func TestNoticeFindings_NeverMoveTheExitCode(t *testing.T) {
	got := noticeFindings(noticeDaemon(t, []notices.Notice{lighterNotice()}, true))
	if n := countFails(got); n != 0 {
		t.Fatalf("countFails = %d, want 0", n)
	}
}

// TestNoticeFindings_InfoSeverityIsNotADoctorRow
//
// PRODUCT CONTRACT (owner ruling, 2026-09-05, on what each surface
// shows). Doctor reports on the health of the setup; a better model
// being available is not a fault in it, and rendering it would have
// meant inventing a fifth status mark for a case none of doctor's four
// describes. It still shows in `waired status` and the tray.
func TestNoticeFindings_InfoSeverityIsNotADoctorRow(t *testing.T) {
	url := noticeDaemon(t, []notices.Notice{
		notices.BetterModel("qwen3-8b-instruct", "qwen3-30b-a3b", 118, 64),
	}, true)

	if got := noticeFindings(url); len(got) != 0 {
		t.Fatalf("got %+v, want no doctor rows for a step-up suggestion", got)
	}
	// …and the same notice does reach the other surface.
	if out := captureStdout(t, func() { printNotices(url) }); !strings.Contains(out, "Better model available") {
		t.Errorf("`waired status` dropped it too:\n%s", out)
	}
}

// TestNoticeFindings_SilentOnAnOlderDaemon
//
// PRODUCT CONTRACT (owner ruling above, and the divergence is
// deliberate): probeObservability emits a StatusSkip on an older daemon
// because the skipped row tells an operator to upgrade to get
// diagnostics back. Here "no notices" and "this daemon does not publish
// notices" have the same thing to show, so a row on every pre-upgrade
// host would be noise.
func TestNoticeFindings_SilentOnAnOlderDaemon(t *testing.T) {
	if got := noticeFindings(noticeDaemon(t, nil, false)); len(got) != 0 {
		t.Fatalf("got %+v, want nothing", got)
	}
	if got := noticeFindings("http://127.0.0.1:65535"); len(got) != 0 {
		t.Fatalf("daemon down: got %+v, want nothing", got)
	}
}

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// TestClaudeLocalWindowFromModels: every shape that is not "waired/default
// carries a positive max_input_tokens" must read as 0 = unknown. Product
// contract — this value decides which value an elevated process removes from a
// machine-wide file, so an ambiguous body must never become a recognised
// number.
func TestClaudeLocalWindowFromModels(t *testing.T) {
	const other = `{"type":"model","id":"claude-waired-cloud[1m]","max_input_tokens":1000000}`
	local := func(tok string) string {
		return fmt.Sprintf(`{"type":"model","id":%q,"max_input_tokens":%s}`, claudePriorWindowModelID, tok)
	}
	cases := []struct {
		name string
		body string
		want int
	}{
		{"local id present", `{"data":[` + other + `,` + local("32768") + `]}`, 32768},
		// The local row states 200704 like every Waired row since
		// waired-agent#1396, so it is not where this computer's own window is.
		{"the local row is not read", `{"data":[{"type":"model","id":"` + claudecode.DirectiveModelLocal + `","max_input_tokens":200704}]}`, 0},
		{"local id first", `{"data":[` + local("8192") + `,` + other + `]}`, 8192},
		{"local id absent", `{"data":[` + other + `]}`, 0},
		{"empty list", `{"data":[]}`, 0},
		{"no data key", `{"has_more":false}`, 0},
		// The gateway omits max_input_tokens (0) when it cannot determine the
		// window — ContextWindowFor's own "fail open" value. It must not become
		// a window here either.
		{"max_input_tokens omitted", `{"data":[{"type":"model","id":"` + claudePriorWindowModelID + `"}]}`, 0},
		{"max_input_tokens zero", `{"data":[` + local("0") + `]}`, 0},
		{"negative is not a window", `{"data":[` + local("-1") + `]}`, 0},
		{"not json", `<html>404</html>`, 0},
		{"empty body", ``, 0},
		{"json but not an object", `[]`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := claudeLocalWindowFromModels([]byte(tc.body)); got != tc.want {
				t.Errorf("claudeLocalWindowFromModels(%s) = %d, want %d", tc.body, got, tc.want)
			}
		})
	}
}

// TestClaudeLocalWindowAt drives the real fetch against a stub listener, so the
// transport half (path, status handling, unreachable host) is exercised rather
// than stubbed out below the behaviour under test.
func TestClaudeLocalWindowAt(t *testing.T) {
	okBody := fmt.Sprintf(`{"data":[{"type":"model","id":%q,"max_input_tokens":32768}]}`, claudePriorWindowModelID)

	t.Run("reads the window from /v1/models", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			fmt.Fprint(w, okBody)
		}))
		defer srv.Close()
		if got := claudeLocalWindowAt(srv.URL); got != 32768 {
			t.Errorf("claudeLocalWindowAt = %d, want 32768", got)
		}
		if gotPath != "/v1/models" {
			t.Errorf("requested %q, want /v1/models", gotPath)
		}
	})

	t.Run("a trailing slash on the base URL still hits /v1/models", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			fmt.Fprint(w, okBody)
		}))
		defer srv.Close()
		if got := claudeLocalWindowAt(srv.URL + "/"); got != 32768 {
			t.Errorf("claudeLocalWindowAt = %d, want 32768", got)
		}
		if gotPath != "/v1/models" {
			t.Errorf("requested %q, want /v1/models (no double slash)", gotPath)
		}
	})

	t.Run("non-200 is unknown", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "nope", http.StatusServiceUnavailable)
		}))
		defer srv.Close()
		if got := claudeLocalWindowAt(srv.URL); got != 0 {
			t.Errorf("claudeLocalWindowAt = %d, want 0 on a 503", got)
		}
	})

	t.Run("unreachable listener is unknown, not a hang", func(t *testing.T) {
		// A closed httptest server gives us a port nothing is listening on.
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close()
		if got := claudeLocalWindowAt(url); got != 0 {
			t.Errorf("claudeLocalWindowAt = %d, want 0 when nothing is listening", got)
		}
	})
}

// TestClaudeWindowStatusLine pins the line that says whether Claude Code is told
// the window every Waired row without "[1m]" is.
//
// Product contract, ratifying source: owner decision 2026-09-16 on
// waired-agent#1396 (the variable is 200704 on every host). When the file says
// anything else, or nothing, the line must say so and name the command that
// fixes it; the elevated-command spelling is platform-correct (waired#752), so
// that case is checked on all three GOOS values. The words themselves are a
// record of today's behaviour, quoted in docs-site.
func TestClaudeWindowStatusLine(t *testing.T) {
	t.Run("no line where no value is expected", func(t *testing.T) {
		for _, c := range []struct {
			name              string
			managed           string
			routed, directive bool
		}{
			{"not routed here", "", false, true},
			{"not routed here, with a value", "200704", false, true},
			{"Waired rows switched off", "", true, false},
		} {
			if got := claudeWindowStatusLine("linux", c.managed, c.routed, c.directive); got != "" {
				t.Errorf("%s: claudeWindowStatusLine = %q, want \"\"", c.name, got)
			}
		}
	})

	t.Run("the fixed value reads as agreement", func(t *testing.T) {
		got := claudeWindowStatusLine("linux", "200704", true, true)
		want := "context window:     200704  (managed settings: 200704)"
		if got != want {
			t.Errorf("claudeWindowStatusLine = %q\nwant                   %q", got, want)
		}
	})

	for _, goos := range []string{"linux", "darwin", "windows"} {
		fix := elevatedCmdline(goos, "waired claude enable")
		t.Run("an unset file names the fix on "+goos, func(t *testing.T) {
			got := claudeWindowStatusLine(goos, "", true, true)
			want := "context window:     200704  (managed settings: not set; re-run `" + fix + "`)"
			if got != want {
				t.Errorf("claudeWindowStatusLine = %q\nwant                   %q", got, want)
			}
		})
		t.Run("a value an older build wrote is stale on "+goos, func(t *testing.T) {
			// 262144 is what a build before waired-agent#1396 wrote on a
			// computer serving that window; 250000 is the pre-#408 constant.
			for _, old := range []string{"262144", "250000"} {
				got := claudeWindowStatusLine(goos, old, true, true)
				want := "context window:     200704  (managed settings: " + old +
					" — stale; Claude Code is being told the wrong window; re-run `" + fix + "`)"
				if got != want {
					t.Errorf("claudeWindowStatusLine = %q\nwant                   %q", got, want)
				}
			}
		})
	}
}

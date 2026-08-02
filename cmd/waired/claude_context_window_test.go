package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
)

// TestClaudeLocalWindowFromModels: every shape that is not "the local directive
// id carries a positive max_input_tokens" must read as 0 = unknown. Product
// contract — this value decides what an elevated process tells Claude Code about
// its context window, so an ambiguous body must never become a written number.
func TestClaudeLocalWindowFromModels(t *testing.T) {
	const other = `{"type":"model","id":"claude-waired-cloud[1m]","max_input_tokens":1000000}`
	local := func(tok string) string {
		return fmt.Sprintf(`{"type":"model","id":%q,"max_input_tokens":%s}`, claudecode.DirectiveModelLocal, tok)
	}
	cases := []struct {
		name string
		body string
		want int
	}{
		{"local id present", `{"data":[` + other + `,` + local("32768") + `]}`, 32768},
		{"local id first", `{"data":[` + local("8192") + `,` + other + `]}`, 8192},
		{"local id absent", `{"data":[` + other + `]}`, 0},
		{"empty list", `{"data":[]}`, 0},
		{"no data key", `{"has_more":false}`, 0},
		// The gateway omits max_input_tokens (0) when it cannot determine the
		// window — ContextWindowFor's own "fail open" value. It must not become
		// a window here either.
		{"max_input_tokens omitted", `{"data":[{"type":"model","id":"` + claudecode.DirectiveModelLocal + `"}]}`, 0},
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
	okBody := fmt.Sprintf(`{"data":[{"type":"model","id":%q,"max_input_tokens":32768}]}`, claudecode.DirectiveModelLocal)

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

// TestClaudeWindowStatusLine pins the one surface that makes a stale window
// visible. Product contract: when the two numbers disagree the line must say so
// and name the command that fixes it — a silent disagreement is the failure
// #408 exists to end. The elevated-command spelling is platform-correct
// (waired#752), so the drift case is checked on all three GOOS values.
func TestClaudeWindowStatusLine(t *testing.T) {
	t.Run("nothing known prints no line", func(t *testing.T) {
		if got := claudeWindowStatusLine("linux", "", 0); got != "" {
			t.Errorf("claudeWindowStatusLine = %q, want \"\"", got)
		}
	})

	t.Run("agreement reports both", func(t *testing.T) {
		got := claudeWindowStatusLine("linux", "32768", 32768)
		if !strings.Contains(got, "32768") || strings.Contains(got, "STALE") {
			t.Errorf("claudeWindowStatusLine = %q, want an agreeing line with no STALE marker", got)
		}
	})

	t.Run("managed settings unset is stated, not implied", func(t *testing.T) {
		got := claudeWindowStatusLine("linux", "", 32768)
		if !strings.Contains(got, "not set") || !strings.Contains(got, "32768") {
			t.Errorf("claudeWindowStatusLine = %q, want it to name the live window and say the file has none", got)
		}
	})

	t.Run("agent down does not imply agreement", func(t *testing.T) {
		got := claudeWindowStatusLine("linux", "250000", 0)
		if !strings.Contains(got, "unknown") || !strings.Contains(got, "250000") {
			t.Errorf("claudeWindowStatusLine = %q, want the written value plus an explicit unknown", got)
		}
		if strings.Contains(got, "STALE") {
			t.Errorf("claudeWindowStatusLine = %q, must not claim staleness it cannot know", got)
		}
	})

	for _, goos := range []string{"linux", "darwin", "windows"} {
		t.Run("drift is called out on "+goos, func(t *testing.T) {
			got := claudeWindowStatusLine(goos, "250000", 32768)
			if !strings.Contains(got, "STALE") || !strings.Contains(got, "250000") || !strings.Contains(got, "32768") {
				t.Errorf("claudeWindowStatusLine = %q, want both numbers and a STALE marker", got)
			}
			want := elevatedCmdline(goos, "waired claude enable")
			if !strings.Contains(got, want) {
				t.Errorf("claudeWindowStatusLine = %q, want the %s recovery command %q", got, goos, want)
			}
		})
	}
}

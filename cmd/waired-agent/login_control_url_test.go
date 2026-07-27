package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/controlurl"
)

// TestResolveDaemonControlURL pins the precedence the app's "Log in…"
// depends on. Product contract: it must match `waired init`'s
// (cmd/waired/main.go), because a device enrolled from the app and one
// enrolled from the CLI have to land on the same Control Plane.
func TestResolveDaemonControlURL(t *testing.T) {
	cases := []struct {
		name            string
		explicit        string
		platformDefault string
		want            string
	}{
		{
			"explicit wins over everything",
			"https://flag.example.com", "https://envfile.example.com",
			"https://flag.example.com",
		},
		{
			"agent.env wins over the built-in default",
			"", "https://envfile.example.com",
			"https://envfile.example.com",
		},
		{
			// The #174 case: a stock install where no installer flag was
			// used and the service manager fed the daemon nothing.
			"built-in default is the last resort",
			"", "",
			controlurl.Default,
		},
		{
			// install.sh / install.ps1 persist the operator's --control
			// string verbatim, so a scheme-less --dev install leaves a
			// bare host in agent.env. The CLI normalizes after resolving;
			// so must the daemon, or setup.Enroll's POST dies on
			// "unsupported protocol scheme".
			"a scheme-less agent.env value is normalized",
			"", "dev.waired.net",
			"https://dev.waired.net",
		},
		{
			"a loopback control plane normalizes to http",
			"127.0.0.1:9477", "",
			"http://127.0.0.1:9477",
		},
		{
			// A typo must not take the whole daemon down, and must not
			// silently win over a good lower tier either.
			"malformed explicit falls through to agent.env",
			"ftp://cp.example.com", "https://envfile.example.com",
			"https://envfile.example.com",
		},
		{
			"malformed agent.env falls through to the built-in default",
			"", "ftp://cp.example.com",
			controlurl.Default,
		},
		{
			"every tier malformed still resolves to the built-in default",
			"ftp://a.example.com", "ftp://b.example.com",
			controlurl.Default,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveDaemonControlURL(c.explicit, c.platformDefault, testLogger())
			if got != c.want {
				t.Errorf("resolveDaemonControlURL(%q, %q) = %q, want %q",
					c.explicit, c.platformDefault, got, c.want)
			}
		})
	}
}

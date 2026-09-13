package main

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/controlurl"
)

// TestResolveDaemonControlURL pins the precedence the app's "Sign in…"
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

// TestDaemonControlURLResolver_ReadsAgentEnvAtEachCall: the resolver reads
// agent.env when it is called, not when it is built, and the daemon's own
// --control / $WAIRED_CONTROL_URL still wins over the file.
func TestDaemonControlURLResolver_ReadsAgentEnvAtEachCall(t *testing.T) {
	file := ""
	resolve := newDaemonControlURLResolver("", func() string { return file }, testLogger())
	if url, src := resolve(); url != controlurl.Default || src != "built-in default" {
		t.Errorf("no agent.env: (%q, %q), want the built-in default", url, src)
	}
	file = "https://app.dev.waired.net"
	if url, src := resolve(); url != "https://app.dev.waired.net" || src != "agent.env" {
		t.Errorf("after agent.env is written: (%q, %q), want its URL", url, src)
	}
	explicit := newDaemonControlURLResolver("https://flag.example", func() string { return file }, testLogger())
	if url, _ := explicit(); url != "https://flag.example" {
		t.Errorf("explicit --control lost to agent.env: %q", url)
	}
}

// TestExplicitControlURL: an environment URL that is only agent.env's copy,
// loaded by systemd's EnvironmentFile, leaves the explicit tier empty; a
// --control flag or a different environment URL stays explicit
// (waired-agent#1377).
func TestExplicitControlURL(t *testing.T) {
	const dev, prod = "https://app.dev.waired.net", "https://app.waired.ai"
	for _, tc := range []struct {
		name       string
		flagValue  string
		flagSet    bool
		env        string
		fileAtBoot string
		want       string
	}{
		{"env is agent.env's copy", dev, false, dev, dev, ""},
		{"same URL after normalization", "app.dev.waired.net/", false, "app.dev.waired.net/", dev + "/", ""},
		{"env differs from agent.env", prod, false, prod, dev, prod},
		{"no agent.env at boot", dev, false, dev, "", dev},
		{"no env", "", false, "", dev, ""},
		{"--control passed, even when it matches agent.env", dev, true, "", dev, dev},
		{"--control passed over a matching env", prod, true, dev, dev, prod},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := explicitControlURL(tc.flagValue, tc.flagSet, tc.env, tc.fileAtBoot); got != tc.want {
				t.Errorf("explicitControlURL(%q, %v, %q, %q) = %q, want %q",
					tc.flagValue, tc.flagSet, tc.env, tc.fileAtBoot, got, tc.want)
			}
		})
	}
}

// TestDaemonControlURLResolver_EnvFromAgentEnvFollowsTheFileEdit: the
// service started with agent.env naming dev, so systemd put dev in the
// environment. Editing the file without a restart moves the next sign-in,
// and removing the line falls back to the built-in default
// (waired-agent#1377).
func TestDaemonControlURLResolver_EnvFromAgentEnvFollowsTheFileEdit(t *testing.T) {
	const dev, prod = "https://app.dev.waired.net", "https://app.waired.ai"
	file := dev
	resolve := newDaemonControlURLResolver(explicitControlURL(dev, false, dev, file), func() string { return file }, testLogger())
	if url, src := resolve(); url != dev || src != "agent.env" {
		t.Errorf("at boot: (%q, %q), want dev from agent.env", url, src)
	}
	file = prod
	if url, src := resolve(); url != prod || src != "agent.env" {
		t.Errorf("after editing agent.env to prod: (%q, %q), want prod from agent.env", url, src)
	}
	file = ""
	if url, src := resolve(); url != controlurl.Default || src != "built-in default" {
		t.Errorf("after removing the line: (%q, %q), want the built-in default", url, src)
	}
}

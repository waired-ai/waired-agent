package hardware

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// fakeEngineEnv turns this test binary into a stand-in engine: when it
// is set, TestMain prints its value and exits 0, so re-executing
// ourselves behaves like `<engine> --version`.
const fakeEngineEnv = "WAIRED_TEST_FAKE_ENGINE_OUTPUT"

// TestMain doubles as that fake engine. EngineVersionAt's entire job is
// to EXECUTE a resolved path, so the only honest test of it spawns a
// real process — and re-executing the test binary is the one way to do
// that identically on linux, darwin and windows (a `#!/bin/sh` stub
// does not run on Windows, and CreateProcess cannot launch a .bat
// directly). The env check happens before m.Run so the child never
// reaches testing's flag parsing, which would reject `--version`.
func TestMain(m *testing.M) {
	if out := os.Getenv(fakeEngineEnv); out != "" {
		fmt.Println(out)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// THE #238 REGRESSION BAR, product contract: an engine version is read
// from the path the caller RESOLVED, with nothing on $PATH. waired's
// own engine lives under the state dir and is deliberately off $PATH,
// and a Windows LocalSystem service inherits no user PATH at all, so a
// probe that needs $PATH reports "no version" on exactly the hosts
// waired provisioned.
//
// It also pins the half of the contract that is easy to get wrong: the
// output is parsed AS THE ENGINE KIND, not as the executable's
// basename. Here the basename is the test binary's, which is not
// "ollama" — a version that comes back proves the two arguments stayed
// separate.
func TestEngineVersionAt_ResolvedPathWithEmptyPATH(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	for _, tc := range []struct {
		engine string
		output string
		want   string
	}{
		// The Warning line is what a fresh install prints when the
		// server is not up yet; ParseEngineVersion skips it.
		{"ollama", "Warning: could not connect to a running Ollama instance\nollama version is 0.31.1", "0.31.1"},
		{"vllm", "0.11.0", "0.11.0"},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			t.Setenv("PATH", "")
			t.Setenv(fakeEngineEnv, tc.output)

			installed, got := EngineVersionAt(context.Background(), tc.engine, self)
			if !installed {
				t.Fatalf("EngineVersionAt(%q, <resolved path>) installed = false, want true "+
					"(the binary is right there; $PATH is not how it was found)", tc.engine)
			}
			if got != tc.want {
				t.Errorf("EngineVersionAt(%q, <resolved path>) = %q, want %q "+
					"(parse must key off the engine kind, not the path basename)", tc.engine, got, tc.want)
			}
		})
	}
}

// A path that is not executable is "not installed" — the caller's
// resolution succeeded but the binary is unusable, which must not be
// reported as an installed engine with an unknown version.
func TestEngineVersionAt_MissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "ollama")
	if installed, ver := EngineVersionAt(context.Background(), "ollama", missing); installed || ver != "" {
		t.Errorf("EngineVersionAt(ollama, %q) = (%v, %q), want (false, \"\")", missing, installed, ver)
	}
	if installed, ver := EngineVersionAt(context.Background(), "ollama", ""); installed || ver != "" {
		t.Errorf("EngineVersionAt(ollama, \"\") = (%v, %q), want (false, \"\")", installed, ver)
	}
}

// Record of today's behaviour, not a contract: the PATH-name fallback
// used by Profilers built without an injected resolver still answers
// "not installed" when the name is not on $PATH. Dropping the
// exec.LookPath pre-check (#238) must not have changed that — the exec
// itself fails the same way.
func TestDefaultEngineVersion_NotOnPATH(t *testing.T) {
	t.Setenv("PATH", "")
	if installed, ver := defaultEngineVersion(context.Background(), "ollama"); installed || ver != "" {
		t.Errorf("defaultEngineVersion(ollama) with an empty PATH = (%v, %q), want (false, \"\")", installed, ver)
	}
}

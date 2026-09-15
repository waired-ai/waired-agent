package claudemanaged

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The uninstall scripts carry a copy of RemoveWithOptions' rules for a host
// whose binary is missing, refused or too old (waired-agent#1398), and
// packaging/install/testdata/claude-leftovers holds the cases every copy is
// run against. A corpus only holds a copy to the rules it has cases for, so
// this test closes the other direction: every literal the Go rules recognise
// must appear in both scripts and in at least one corpus input. Adding a
// marker or an owned key here therefore fails until the scripts and the corpus
// know it too.
//
// PIN: record of today's behaviour -- the literal list is what
// RemoveWithOptions and hook.go recognise on 2026-09-16.

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func managedCorpusInputs(t *testing.T) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	dir := filepath.Join(filepath.Dir(file), "..", "..", "..", "packaging", "install", "testdata", "claude-leftovers", "managed")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name(), "input.json"))
		if err != nil {
			t.Fatal(err)
		}
		all.Write(b)
	}
	return all.String()
}

func TestUninstallScriptsKnowEveryManagedOwnershipLiteral(t *testing.T) {
	literals := []string{
		baseURLKey, loopbackPrefix,
		discoveryKey,
		autoCompactWindowKey, legacyAutoCompactWindowValue,
		maxContextTokensKey, legacyDirectivesMaxContextTokensValue,
		subagentModelKey, SubagentModelID,
		stopHookEvent, sessionStartHookEvent,
	}
	literals = append(literals, stopHookMarkers...)
	literals = append(literals, refreshHookMarkers...)

	scripts := map[string]string{
		"packaging/install/uninstall.ps1": repoFile(t, "packaging/install/uninstall.ps1"),
		"packaging/install/uninstall.sh":  repoFile(t, "packaging/install/uninstall.sh"),
	}
	corpus := managedCorpusInputs(t)
	for _, lit := range literals {
		for name, body := range scripts {
			if !strings.Contains(body, lit) {
				t.Errorf("%s doesn't know %q, which RemoveWithOptions recognises (waired-agent#1398)", name, lit)
			}
		}
		if !strings.Contains(corpus, lit) {
			t.Errorf("no managed corpus case carries %q; add one to packaging/install/testdata/claude-leftovers/managed", lit)
		}
	}
}

// TestEverythingWriteAddsIsSomethingTheScriptsRemove runs the fullest write
// waired does and checks each env key and hook command it produces against
// the scripts. A key Write starts adding without a matching removal rule is
// the leftover the scripts could never clean up.
func TestEverythingWriteAddsIsSomethingTheScriptsRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managed-settings.json")
	restore := SwapPathForTest(path)
	defer restore()
	if _, err := WriteWithOptions("http://127.0.0.1:9472", WriteOptions{
		ModelRouteDirectives: true, LocalContextWindow: 250000, ModelPeerEntries: 3,
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Env   map[string]string `json:"env"`
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	ps1 := repoFile(t, "packaging/install/uninstall.ps1")
	sh := repoFile(t, "packaging/install/uninstall.sh")
	for key := range doc.Env {
		if !strings.Contains(ps1, key) || !strings.Contains(sh, key) {
			t.Errorf("Write adds env %s, which an uninstall script doesn't know to remove", key)
		}
	}
	for event, entries := range doc.Hooks {
		for _, e := range entries {
			for _, h := range e.Hooks {
				known := false
				for _, m := range append(append([]string{}, stopHookMarkers...), refreshHookMarkers...) {
					if strings.Contains(h.Command, m) && strings.Contains(ps1, m) && strings.Contains(sh, m) {
						known = true
					}
				}
				if !known {
					t.Errorf("Write adds a %s hook %q that no uninstall script recognises", event, h.Command)
				}
			}
		}
	}
}

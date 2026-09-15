package installscripts

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/claudecode"
	"github.com/waired-ai/waired-agent/internal/integration/claudemanaged"
)

// The Claude Code settings Waired leaves behind are removed by four
// implementations: the Go code `waired claude disable` runs, and the copies
// in uninstall.ps1 (PowerShell) and uninstall.sh (Python on Linux, JavaScript
// for Automation on macOS) that run when the binary is missing, cannot start,
// or is too old to know a newer rule (waired-agent#1398).
//
// packaging/install/testdata/claude-leftovers is what keeps them the same
// rule. This test holds the Go side to it; installtest-pwsh.ps1,
// installtest-windows.ps1, installtest-dash.sh and installtest-macos.sh hold
// the script copies to the same files. A case added there without a Go
// answer, or a Go change that moves an answer, fails here first.
//
// PIN: record of today's Go behaviour. The expected files are what the Go
// removers do on 2026-09-16; the scripts copy that, they do not define it.

const leftoversCorpus = "packaging/install/testdata/claude-leftovers"

// leftoverCase is one directory of the corpus.
type leftoverCase struct {
	name  string
	dir   string
	input []byte
}

func leftoverCases(t *testing.T, kind string) []leftoverCase {
	t.Helper()
	root := filepath.Join(repoRoot(t), filepath.FromSlash(leftoversCorpus), kind)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read corpus %s: %v", kind, err)
	}
	var cases []leftoverCase
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		input, err := os.ReadFile(filepath.Join(dir, "input.json"))
		if err != nil {
			t.Fatalf("%s/%s: %v", kind, e.Name(), err)
		}
		cases = append(cases, leftoverCase{name: e.Name(), dir: dir, input: input})
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].name < cases[j].name })
	if len(cases) == 0 {
		t.Fatalf("corpus %s has no cases", kind)
	}
	return cases
}

// checkLeftoverResult compares the file at path, after a remover ran, with
// the case's single expected.* file. emptyIsAbsent treats a file holding `{}`
// as absent: the per-user removers disagree among themselves about whether an
// emptied settings file is deleted or written as `{}`, and Claude Code reads
// the two the same way.
func checkLeftoverResult(t *testing.T, c leftoverCase, path string, emptyIsAbsent bool) {
	t.Helper()
	got, readErr := os.ReadFile(path)
	absent := os.IsNotExist(readErr)
	if readErr != nil && !absent {
		t.Fatalf("read result: %v", readErr)
	}
	if !absent && emptyIsAbsent {
		var obj map[string]any
		if json.Unmarshal(bytes.TrimPrefix(got, []byte{0xEF, 0xBB, 0xBF}), &obj) == nil && obj != nil && len(obj) == 0 {
			absent = true
		}
	}
	have := func(name string) bool {
		_, err := os.Stat(filepath.Join(c.dir, name))
		return err == nil
	}
	switch {
	case have("expected.absent"):
		if !absent {
			t.Errorf("want the file removed, got:\n%s", got)
		}
	case have("expected.unchanged"):
		if absent {
			t.Fatalf("want the file untouched, it was removed")
		}
		if !bytes.Equal(got, c.input) {
			t.Errorf("want the file byte-for-byte untouched, got:\n%s", got)
		}
	case have("expected.json"):
		if absent {
			t.Fatalf("want the file rewritten, it was removed")
		}
		want, err := os.ReadFile(filepath.Join(c.dir, "expected.json"))
		if err != nil {
			t.Fatal(err)
		}
		var g, w any
		if err := json.Unmarshal(got, &g); err != nil {
			t.Fatalf("result is not JSON: %v\n%s", err, got)
		}
		if err := json.Unmarshal(want, &w); err != nil {
			t.Fatalf("expected.json is not JSON: %v", err)
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("result differs from expected.json\n got: %s\nwant: %s", got, want)
		}
	default:
		t.Fatalf("%s has no expected.json, expected.absent or expected.unchanged", c.dir)
	}
}

func TestClaudeLeftoversCorpusManagedGo(t *testing.T) {
	for _, c := range leftoverCases(t, "managed") {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "managed-settings.json")
			if err := os.WriteFile(path, c.input, 0o644); err != nil {
				t.Fatal(err)
			}
			restore := claudemanaged.SwapPathForTest(path)
			defer restore()
			// No window: the case every uninstall without a running service is
			// in, and the only one the scripts can be in.
			if _, err := claudemanaged.RemoveWithOptions(claudemanaged.RemoveOptions{}); err != nil {
				t.Fatalf("RemoveWithOptions: %v", err)
			}
			checkLeftoverResult(t, c, path, false)
		})
	}
}

func TestClaudeLeftoversCorpusUserSettingsGo(t *testing.T) {
	for _, c := range leftoverCases(t, "user-settings") {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			path := claudecode.SettingsPath(home)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, c.input, 0o644); err != nil {
				t.Fatal(err)
			}
			// The per-user steps of runClaudeDisable (cmd/waired/claude.go), in
			// its order. Their errors are warnings there, so they are ignored
			// here too: a step that refuses a file leaves it for the next one.
			_ = claudecode.RemoveRouteSkill(home)
			_ = claudecode.RemoveStatusLine(home)
			_, _ = claudecode.RemovePickerLineup(path)
			_ = claudecode.RemoveModelSetting(home)
			_, _ = claudecode.SetSubagentPlacement(path, claudecode.SubagentFollow, "")
			checkLeftoverResult(t, c, path, true)
		})
	}
}

func TestClaudeLeftoversCorpusRetiredCacheGo(t *testing.T) {
	for _, c := range leftoverCases(t, "retired-cache") {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			path := claudecode.RetiredCachePath("", home)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, c.input, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := claudecode.RemoveRetiredCacheOwned("", home); err != nil {
				t.Fatalf("RemoveRetiredCacheOwned: %v", err)
			}
			checkLeftoverResult(t, c, path, false)
		})
	}
}

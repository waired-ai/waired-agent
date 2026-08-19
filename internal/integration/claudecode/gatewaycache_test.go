package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func fixedNow() func() time.Time {
	t := time.Date(2026, 8, 2, 9, 8, 7, 654_000_000, time.UTC)
	return func() time.Time { return t }
}

// fixedNowMillis is the epoch-millisecond value fixedNow renders to.
const fixedNowMillis = int64(1785661687654)

// TestGatewayCachePath: CLAUDE_CONFIG_DIR relocates the whole tree, else it is
// ~/.claude — the rule Claude Code itself applies, and the one our writer has
// to mirror or it writes where nothing reads. Untagged resolution, so all three
// OSes are covered from one host (CLAUDE.md §Cross-OS parity).
func TestGatewayCachePath(t *testing.T) {
	cases := []struct {
		name      string
		configDir string
		home      string
		want      []string // path elements, joined per-OS
	}{
		{"home default", "", filepath.Join("u", "me"), []string{"u", "me", ".claude", "cache", "gateway-models.json"}},
		{"config dir wins", filepath.Join("x", "cfg"), filepath.Join("u", "me"), []string{"x", "cfg", "cache", "gateway-models.json"}},
		{"config dir wins even with no home", filepath.Join("x", "cfg"), "", []string{"x", "cfg", "cache", "gateway-models.json"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := filepath.Join(tc.want...)
			if got := GatewayCachePath(tc.configDir, tc.home); got != want {
				t.Errorf("GatewayCachePath(%q, %q) = %q, want %q", tc.configDir, tc.home, got, want)
			}
		})
	}
}

// TestWriteGatewayCacheSchema pins the on-disk shape byte for byte against what
// Claude Code writes itself. Product contract, and an unforgiving one: the
// reader schema-parses the whole document, so a wrong TYPE anywhere yields null
// and the picker silently falls back to the built-in list — the exact symptom
// #407 exists to remove, with no error anywhere to say so.
//
// Measured against claude 2.1.220 and re-measured per release by
// scripts/ci/canary-cache-schema.py.
func TestWriteGatewayCacheSchema(t *testing.T) {
	home := t.TempDir()
	const baseURL = "http://127.0.0.1:9472"
	path, err := WriteGatewayCache("", home, baseURL, DirectiveCacheModels(), fixedNow())
	if err != nil {
		t.Fatalf("WriteGatewayCache: %v", err)
	}
	if want := GatewayCachePath("", home); path != want {
		t.Errorf("returned path %q, want %q", path, want)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the document must parse as JSON: %v", err)
	}

	// Exactly these three keys: the reader strips anything else, so extra keys
	// are dead weight and a missing one is a broken document.
	wantKeys := map[string]bool{"baseUrl": true, "fetchedAt": true, "models": true}
	for k := range doc {
		if !wantKeys[k] {
			t.Errorf("unexpected top-level key %q", k)
		}
	}
	for k := range wantKeys {
		if _, ok := doc[k]; !ok {
			t.Errorf("missing top-level key %q", k)
		}
	}

	if doc["baseUrl"] != baseURL {
		t.Errorf("baseUrl = %v, want %q (the reader compares it by exact string)", doc["baseUrl"], baseURL)
	}
	// json.Unmarshal into any gives float64 for every number; the point is that
	// it is a NUMBER, and epoch milliseconds rather than seconds or RFC3339.
	fetched, ok := doc["fetchedAt"].(float64)
	if !ok {
		t.Fatalf("fetchedAt is %T, want a JSON number (epoch ms)", doc["fetchedAt"])
	}
	if int64(fetched) != fixedNowMillis {
		t.Errorf("fetchedAt = %d, want %d (epoch milliseconds)", int64(fetched), fixedNowMillis)
	}

	models, ok := doc["models"].([]any)
	if !ok {
		t.Fatalf("models is %T, want an array", doc["models"])
	}
	if len(models) != len(DirectiveCacheModels()) {
		t.Fatalf("models has %d entries, want %d", len(models), len(DirectiveCacheModels()))
	}
	for i, raw := range models {
		m, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("models[%d] is %T, want an object", i, raw)
		}
		for k := range m {
			if k != "id" && k != "display_name" {
				t.Errorf("models[%d] has unexpected key %q", i, k)
			}
		}
		want := DirectiveCacheModels()[i]
		if m["id"] != want.ID {
			t.Errorf("models[%d].id = %v, want %q", i, m["id"], want.ID)
		}
		if m["display_name"] != want.DisplayName {
			t.Errorf("models[%d].display_name = %v, want %q", i, m["display_name"], want.DisplayName)
		}
	}
}

// TestWriteGatewayCacheMode: 0600, matching what Claude Code writes. Skipped on
// Windows, where mode bits are not the access-control mechanism (the secrets
// package applies a DACL there instead).
func TestWriteGatewayCacheMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not the ACL on Windows; secrets.WriteSecret applies a DACL there")
	}
	home := t.TempDir()
	path, err := WriteGatewayCache("", home, "http://127.0.0.1:9472", DirectiveCacheModels(), fixedNow())
	if err != nil {
		t.Fatalf("WriteGatewayCache: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600", got)
	}
}

// TestWriteGatewayCacheCreatesTheCacheDir: a host that has never run Claude Code
// has no cache/ directory, and that is exactly the fresh-install case.
func TestWriteGatewayCacheCreatesTheCacheDir(t *testing.T) {
	home := filepath.Join(t.TempDir(), "nonexistent", "home")
	if _, err := WriteGatewayCache("", home, "http://127.0.0.1:9472", DirectiveCacheModels(), fixedNow()); err != nil {
		t.Fatalf("WriteGatewayCache: %v", err)
	}
	if _, err := os.Stat(GatewayCachePath("", home)); err != nil {
		t.Errorf("cache not written into a fresh tree: %v", err)
	}
}

// TestWriteGatewayCacheHonoursConfigDir: with CLAUDE_CONFIG_DIR set, the cache
// must land there and NOT in ~/.claude — writing to the home path would leave
// the file where that user's Claude Code never looks.
func TestWriteGatewayCacheHonoursConfigDir(t *testing.T) {
	home := t.TempDir()
	configDir := t.TempDir()
	if _, err := WriteGatewayCache(configDir, home, "http://127.0.0.1:9472", DirectiveCacheModels(), fixedNow()); err != nil {
		t.Fatalf("WriteGatewayCache: %v", err)
	}
	if _, err := os.Stat(GatewayCachePath(configDir, home)); err != nil {
		t.Errorf("cache not written under CLAUDE_CONFIG_DIR: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "cache", gatewayCacheFile)); err == nil {
		t.Error("cache was also written under ~/.claude; CLAUDE_CONFIG_DIR must be the only target")
	}
}

// TestWriteGatewayCacheRefusesUselessDocuments: both refusals produce a document
// that parses fine and yields an empty or ignored picker — a silent failure, so
// they are errors rather than writes.
func TestWriteGatewayCacheRefusesUselessDocuments(t *testing.T) {
	t.Run("empty base URL", func(t *testing.T) {
		home := t.TempDir()
		if _, err := WriteGatewayCache("", home, "", DirectiveCacheModels(), fixedNow()); err == nil {
			t.Error("want an error for an empty baseUrl")
		}
		if _, err := os.Stat(GatewayCachePath("", home)); err == nil {
			t.Error("a refused write must leave no file behind")
		}
	})
	t.Run("no models", func(t *testing.T) {
		home := t.TempDir()
		if _, err := WriteGatewayCache("", home, "http://127.0.0.1:9472", nil, fixedNow()); err == nil {
			t.Error("want an error for an empty model list")
		}
		if _, err := os.Stat(GatewayCachePath("", home)); err == nil {
			t.Error("a refused write must leave no file behind")
		}
	})
}

// TestWriteGatewayCacheOverwrites: re-running enable after a port change must
// replace the document, not merge into it — a stale baseUrl disables the cache
// entirely on the read side.
func TestWriteGatewayCacheOverwrites(t *testing.T) {
	home := t.TempDir()
	if _, err := WriteGatewayCache("", home, "http://127.0.0.1:9472", DirectiveCacheModels(), fixedNow()); err != nil {
		t.Fatalf("first write: %v", err)
	}
	path, err := WriteGatewayCache("", home, "http://127.0.0.1:9999", DirectiveCacheModels(), fixedNow())
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["baseUrl"] != "http://127.0.0.1:9999" {
		t.Errorf("baseUrl = %v, want the second write's value", doc["baseUrl"])
	}
}

// TestRemoveGatewayCache: removal is what `waired claude disable` and the
// directives opt-out rely on, and an absent file is success — disable must not
// fail on a host that never had one.
func TestRemoveGatewayCache(t *testing.T) {
	t.Run("removes a written cache", func(t *testing.T) {
		home := t.TempDir()
		if _, err := WriteGatewayCache("", home, "http://127.0.0.1:9472", DirectiveCacheModels(), fixedNow()); err != nil {
			t.Fatalf("WriteGatewayCache: %v", err)
		}
		if err := RemoveGatewayCache("", home); err != nil {
			t.Fatalf("RemoveGatewayCache: %v", err)
		}
		if _, err := os.Stat(GatewayCachePath("", home)); err == nil {
			t.Error("cache still present after RemoveGatewayCache")
		}
	})

	t.Run("absent is success", func(t *testing.T) {
		if err := RemoveGatewayCache("", t.TempDir()); err != nil {
			t.Errorf("RemoveGatewayCache on a host with no cache: %v", err)
		}
	})

	t.Run("honours CLAUDE_CONFIG_DIR", func(t *testing.T) {
		home := t.TempDir()
		configDir := t.TempDir()
		if _, err := WriteGatewayCache(configDir, home, "http://127.0.0.1:9472", DirectiveCacheModels(), fixedNow()); err != nil {
			t.Fatalf("WriteGatewayCache: %v", err)
		}
		if err := RemoveGatewayCache(configDir, home); err != nil {
			t.Fatalf("RemoveGatewayCache: %v", err)
		}
		if _, err := os.Stat(GatewayCachePath(configDir, home)); err == nil {
			t.Error("cache still present under CLAUDE_CONFIG_DIR after removal")
		}
	})
}

// TestDirectiveIdsSurvivePickerFilter: Claude Code filters discovered ids to
// ^(claude|anthropic)/i before showing them. An id that fails it is dropped
// silently, so every id we write has to pass — this is why the branded
// directive ids exist instead of raw catalog ids.
func TestDirectiveIdsSurvivePickerFilter(t *testing.T) {
	for _, m := range DirectiveCacheModels() {
		lower := m.ID
		if len(lower) < 6 {
			t.Errorf("id %q is implausibly short", m.ID)
			continue
		}
		if !hasFoldPrefix(lower, "claude") && !hasFoldPrefix(lower, "anthropic") {
			t.Errorf("id %q does not match ^(claude|anthropic)/i and would be dropped from the picker", m.ID)
		}
		if m.DisplayName == "" {
			t.Errorf("id %q has no display name; the picker would render it bare", m.ID)
		}
	}
}

// ReadGatewayCache exists so a diagnostic can report what the CLIENT will
// make of the file, so its three answers have to be distinguishable:
// absent (nothing written for this user), unreadable (something is there and
// Claude Code will read it as an empty picker), and present.
//
// PIN: record of the measured on-disk contract in this file's header — a
// wrong-typed fetchedAt makes the whole document parse to null client-side,
// which is why a parse failure must not be reported as "absent".
func TestReadGatewayCache(t *testing.T) {
	const baseURL = "http://127.0.0.1:9472"

	t.Run("absent is not an error", func(t *testing.T) {
		home := t.TempDir()
		st, err := ReadGatewayCache("", home)
		if err != nil {
			t.Fatalf("ReadGatewayCache: %v", err)
		}
		if st.Present {
			t.Error("an empty home must not report a present cache")
		}
		if st.Path != GatewayCachePath("", home) {
			t.Errorf("Path = %q, want the path it looked at", st.Path)
		}
	})

	t.Run("round-trips what the writer wrote", func(t *testing.T) {
		home := t.TempDir()
		if _, err := WriteGatewayCache("", home, baseURL, DirectiveCacheModels(), fixedNow()); err != nil {
			t.Fatalf("WriteGatewayCache: %v", err)
		}
		st, err := ReadGatewayCache("", home)
		if err != nil {
			t.Fatalf("ReadGatewayCache: %v", err)
		}
		if !st.Present {
			t.Fatal("the file is there; Present must be true")
		}
		if st.BaseURL != baseURL {
			t.Errorf("BaseURL = %q, want %q", st.BaseURL, baseURL)
		}
		if got, want := len(st.Models), len(DirectiveCacheModels()); got != want {
			t.Errorf("read %d models, want %d", got, want)
		}
		if st.FetchedAt.UnixMilli() != fixedNow()().UnixMilli() {
			t.Errorf("FetchedAt = %v, want the written timestamp %v", st.FetchedAt, fixedNow()())
		}
	})

	t.Run("a malformed document is an error, not an absence", func(t *testing.T) {
		home := t.TempDir()
		path := GatewayCachePath("", home)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		st, err := ReadGatewayCache("", home)
		if err == nil {
			t.Fatal("a truncated document must be reported, not silently read as absent")
		}
		if st.Present {
			t.Error("Present must stay false when the document did not parse")
		}
	})
}

func hasFoldPrefix(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		a, b := s[i], prefix[i]
		if 'A' <= a && a <= 'Z' {
			a += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}

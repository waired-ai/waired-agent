package claudecode

import (
	"os"
	"path/filepath"
	"testing"
)

const liveBaseURL = "http://127.0.0.1:9472"

func seedRetiredCache(t *testing.T, body string) (home, path string) {
	t.Helper()
	home = t.TempDir()
	path = RetiredCachePath("", home)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return home, path
}

// TestRemoveRetiredCache is the upgrade path for waired-agent#1185. Until it,
// the Waired rows reached the picker through Claude Code's own discovery
// cache; a host that upgrades still has that file, and while the discovery
// flag survives (it is scrubbed only by a root enable) the picker shows the
// stale rows beside the new ones — measured on 2.1.261, 2026-09-06.
func TestRemoveRetiredCache(t *testing.T) {
	// What a pre-#1185 waired actually wrote, copied off a real host.
	ours := `{"baseUrl":"` + liveBaseURL + `","fetchedAt":1788458345856,"models":[` +
		`{"id":"claude-waired-auto","display_name":"Waired — 200k (any of your devices)"},` +
		`{"id":"anthropic-waired-local","display_name":"Waired local (this device)"},` +
		`{"id":"claude-waired-peer","display_name":"Waired peer (another device)"}]}`

	t.Run("ours is taken away", func(t *testing.T) {
		home, path := seedRetiredCache(t, ours)
		removed, err := RemoveRetiredCache("", home, liveBaseURL)
		if err != nil || !removed {
			t.Fatalf("RemoveRetiredCache = (%v, %v), want (true, nil)", removed, err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the file is still there (stat err = %v)", err)
		}
	})

	// Everything below is left alone, and none of it is an error: this runs
	// inside a SessionStart hook on every `claude` launch.
	for _, tc := range []struct {
		name string
		body string
		base string
	}{
		{"a cache naming a different gateway", ours, "http://127.0.0.1:9999"},
		{"a cache holding a model that is not ours",
			`{"baseUrl":"` + liveBaseURL + `","models":[{"id":"claude-opus-4-8"}]}`, liveBaseURL},
		{"one waired id among somebody else's",
			`{"baseUrl":"` + liveBaseURL + `","models":[{"id":"waired"},{"id":"claude-opus-4-8"}]}`, liveBaseURL},
		{"an empty model list", `{"baseUrl":"` + liveBaseURL + `","models":[]}`, liveBaseURL},
		{"not JSON", `{`, liveBaseURL},
		{"no live base URL to compare against", ours, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, path := seedRetiredCache(t, tc.body)
			removed, err := RemoveRetiredCache("", home, tc.base)
			if err != nil || removed {
				t.Fatalf("RemoveRetiredCache = (%v, %v), want (false, nil)", removed, err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Errorf("the file was deleted anyway: %v", err)
			}
		})
	}

	t.Run("no file at all", func(t *testing.T) {
		removed, err := RemoveRetiredCache("", t.TempDir(), liveBaseURL)
		if err != nil || removed {
			t.Errorf("RemoveRetiredCache = (%v, %v), want (false, nil)", removed, err)
		}
	})

	// CLAUDE_CONFIG_DIR relocates the whole tree, this file with it.
	t.Run("under CLAUDE_CONFIG_DIR", func(t *testing.T) {
		cfg := t.TempDir()
		path := RetiredCachePath(cfg, "/home/ignored")
		if want := filepath.Join(cfg, "cache", "gateway-models.json"); path != want {
			t.Fatalf("path = %q, want %q", path, want)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(ours), 0o600); err != nil {
			t.Fatal(err)
		}
		if removed, err := RemoveRetiredCache(cfg, "/home/ignored", liveBaseURL); err != nil || !removed {
			t.Fatalf("RemoveRetiredCache = (%v, %v), want (true, nil)", removed, err)
		}
	})
}

// TestRemoveRetiredCacheOwned is the cache half of waired-agent#1398. `waired
// claude disable` reaches the per-user cleanup after it has already taken
// ANTHROPIC_BASE_URL out of managed settings, so the exact-match rule above
// had nothing to match and the cache stayed on every host. The broader corpus
// (packaging/install/testdata/claude-leftovers/retired-cache) is run against
// this function from scripts/install; these cases pin the edges that corpus
// does not name.
func TestRemoveRetiredCacheOwned(t *testing.T) {
	ours := `{"baseUrl":"http://127.0.0.1:9472","models":[{"id":"claude-waired-auto"},{"id":"anthropic-waired-local"}]}`
	for _, tc := range []struct {
		name string
		body string
		want bool
	}{
		{"a loopback cache of Waired ids, with no live base URL to ask", ours, true},
		{"a loopback cache on another port", `{"baseUrl":"http://127.0.0.1:1","models":[{"id":"waired"}]}`, true},
		{"localhost is not the prefix waired writes", `{"baseUrl":"http://localhost:9472","models":[{"id":"waired"}]}`, false},
		{"a model that is not ours", `{"baseUrl":"http://127.0.0.1:9472","models":[{"id":"claude-opus-4-8"}]}`, false},
		{"no models", `{"baseUrl":"http://127.0.0.1:9472","models":[]}`, false},
		{"not JSON", `{`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, path := seedRetiredCache(t, tc.body)
			removed, err := RemoveRetiredCacheOwned("", home)
			if err != nil || removed != tc.want {
				t.Fatalf("RemoveRetiredCacheOwned = (%v, %v), want (%v, nil)", removed, err, tc.want)
			}
			_, statErr := os.Stat(path)
			if gone := os.IsNotExist(statErr); gone != tc.want {
				t.Errorf("file gone = %v, want %v", gone, tc.want)
			}
		})
	}
	t.Run("no file at all", func(t *testing.T) {
		if removed, err := RemoveRetiredCacheOwned("", t.TempDir()); err != nil || removed {
			t.Errorf("RemoveRetiredCacheOwned = (%v, %v), want (false, nil)", removed, err)
		}
	})
}

// TestRemoveRetiredFallbackMarkers: the retired Stop hook kept a marker per
// session under the user's cache directory, and its writer was deleted in #1184
// without anything to take the directory away (waired-agent#1398).
func TestRemoveRetiredFallbackMarkers(t *testing.T) {
	t.Run("the markers go, and the emptied parent with them", func(t *testing.T) {
		cache := t.TempDir()
		dir := filepath.Join(cache, "waired", "claude-fallback")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "9c4870ee"), []byte("2"), 0o600); err != nil {
			t.Fatal(err)
		}
		removed, err := RemoveRetiredFallbackMarkers(cache)
		if err != nil || !removed {
			t.Fatalf("RemoveRetiredFallbackMarkers = (%v, %v), want (true, nil)", removed, err)
		}
		if _, err := os.Stat(filepath.Join(cache, "waired")); !os.IsNotExist(err) {
			t.Errorf("the emptied waired cache directory is still there (stat err = %v)", err)
		}
	})
	t.Run("anything else in the parent keeps it", func(t *testing.T) {
		cache := t.TempDir()
		if err := os.MkdirAll(filepath.Join(cache, "waired", "claude-fallback"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, "waired", "other"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := RemoveRetiredFallbackMarkers(cache); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(cache, "waired", "other")); err != nil {
			t.Errorf("a sibling was removed: %v", err)
		}
	})
	t.Run("nothing there, or no cache directory at all", func(t *testing.T) {
		for _, cache := range []string{t.TempDir(), ""} {
			if removed, err := RemoveRetiredFallbackMarkers(cache); err != nil || removed {
				t.Errorf("RemoveRetiredFallbackMarkers(%q) = (%v, %v), want (false, nil)", cache, removed, err)
			}
		}
	})
}

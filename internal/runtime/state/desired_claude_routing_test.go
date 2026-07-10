package state

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadDesiredClaudeRoutingMissingReturnsDefault(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadDesiredClaudeRouting(dir)
	if err != nil {
		t.Fatalf("ReadDesiredClaudeRouting: %v", err)
	}
	if want := DefaultClaudeRoutingPolicy(); got != want {
		t.Fatalf("missing file: got %+v, want %+v", got, want)
	}
	if got.Main != ClaudeRouteAuto || got.Sub != ClaudeRouteSame {
		t.Fatalf("default should be main=auto sub=same, got %+v", got)
	}
}

func TestDesiredClaudeRoutingRoundTrip(t *testing.T) {
	for _, p := range []ClaudeRoutingPolicy{
		{Main: ClaudeRouteAuto, Sub: ClaudeRouteSame},
		{Main: ClaudeRouteWaired, Sub: ClaudeRouteSame},
		{Main: ClaudeRouteAnthropic, Sub: ClaudeRouteWaired},
		{Main: ClaudeRouteAuto, Sub: ClaudeRouteAnthropic},
		{Main: ClaudeRouteWaired, Sub: ClaudeRouteAuto},
	} {
		dir := t.TempDir()
		if err := WriteDesiredClaudeRouting(dir, p); err != nil {
			t.Fatalf("write %+v: %v", p, err)
		}
		got, err := ReadDesiredClaudeRouting(dir)
		if err != nil {
			t.Fatalf("read %+v: %v", p, err)
		}
		if got != p {
			t.Fatalf("round trip: got %+v, want %+v", got, p)
		}
	}
}

func TestReadDesiredClaudeRoutingCoercesPartialFile(t *testing.T) {
	dir := t.TempDir()
	// main empty → auto, sub empty → same.
	writeRaw(t, dir, `{}`)
	got, err := ReadDesiredClaudeRouting(dir)
	if err != nil {
		t.Fatalf("read empty object: %v", err)
	}
	if got.Main != ClaudeRouteAuto || got.Sub != ClaudeRouteSame {
		t.Fatalf("empty object: got %+v, want main=auto sub=same", got)
	}
	// main="same" is subagent-only → coerced to auto.
	writeRaw(t, dir, `{"main":"same","sub":"waired"}`)
	got, err = ReadDesiredClaudeRouting(dir)
	if err != nil {
		t.Fatalf("read main=same: %v", err)
	}
	if got.Main != ClaudeRouteAuto || got.Sub != ClaudeRouteWaired {
		t.Fatalf("main=same: got %+v, want main=auto sub=waired", got)
	}
}

func TestReadDesiredClaudeRoutingRejectsUnknownClass(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, dir, `{"main":"openai","sub":"same"}`)
	if _, err := ReadDesiredClaudeRouting(dir); err == nil {
		t.Fatal("expected error for unknown main class")
	}
}

func TestReadDesiredClaudeRoutingMalformedErrors(t *testing.T) {
	dir := t.TempDir()
	writeRaw(t, dir, `{not json`)
	if _, err := ReadDesiredClaudeRouting(dir); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestClaudeRoutingPolicyEffective(t *testing.T) {
	cases := []struct {
		pol       ClaudeRoutingPolicy
		main, sub ClaudeRouteClass
	}{
		{ClaudeRoutingPolicy{Main: ClaudeRouteAuto, Sub: ClaudeRouteSame}, ClaudeRouteAuto, ClaudeRouteAuto},
		{ClaudeRoutingPolicy{Main: ClaudeRouteAnthropic, Sub: ClaudeRouteSame}, ClaudeRouteAnthropic, ClaudeRouteAnthropic},
		{ClaudeRoutingPolicy{Main: ClaudeRouteAnthropic, Sub: ClaudeRouteWaired}, ClaudeRouteAnthropic, ClaudeRouteWaired},
		{ClaudeRoutingPolicy{Main: ClaudeRouteWaired, Sub: ClaudeRouteAuto}, ClaudeRouteWaired, ClaudeRouteAuto},
		{ClaudeRoutingPolicy{}, ClaudeRouteAuto, ClaudeRouteAuto}, // zero value
	}
	for _, c := range cases {
		if got := c.pol.Effective(ClaudeClassMain); got != c.main {
			t.Errorf("%+v Effective(main)=%q, want %q", c.pol, got, c.main)
		}
		if got := c.pol.Effective(ClaudeClassSub); got != c.sub {
			t.Errorf("%+v Effective(sub)=%q, want %q", c.pol, got, c.sub)
		}
	}
}

func TestMigrateDesiredClaudeRouting(t *testing.T) {
	cases := []struct {
		name  string
		route string // desired-claude-route JSON, "" = absent
		node  string // desired-claude-node JSON, "" = absent
		want  ClaudeRoutingPolicy
	}{
		{
			name:  "auto+all-local → auto/same",
			route: `{"mode":"auto","allow_fallback":true}`,
			node:  `{"main":{"kind":"local"},"sub":{"kind":"local"}}`,
			want:  ClaudeRoutingPolicy{Main: ClaudeRouteAuto, Sub: ClaudeRouteSame},
		},
		{
			name:  "route=local → waired/same",
			route: `{"mode":"local"}`,
			node:  `{"main":{"kind":"local"},"sub":{"kind":"local"}}`,
			want:  ClaudeRoutingPolicy{Main: ClaudeRouteWaired, Sub: ClaudeRouteSame},
		},
		{
			name:  "route=anthropic → anthropic/same",
			route: `{"mode":"anthropic"}`,
			node:  `{"main":{"kind":"local"},"sub":{"kind":"local"}}`,
			want:  ClaudeRoutingPolicy{Main: ClaudeRouteAnthropic, Sub: ClaudeRouteSame},
		},
		{
			name:  "hybrid main=anthropic/sub=local under auto",
			route: `{"mode":"auto","allow_fallback":true}`,
			node:  `{"main":{"kind":"anthropic"},"sub":{"kind":"local"}}`,
			want:  ClaudeRoutingPolicy{Main: ClaudeRouteAnthropic, Sub: ClaudeRouteAuto},
		},
		{
			name:  "node peer → waired (worker owns node choice)",
			route: `{"mode":"auto","allow_fallback":true}`,
			node:  `{"main":{"kind":"peer","peer_device_id":"dev-x"},"sub":{"kind":"local"}}`,
			want:  ClaudeRoutingPolicy{Main: ClaudeRouteWaired, Sub: ClaudeRouteAuto},
		},
		{
			name:  "auto + fallback off → waired (privacy opt-out)",
			route: `{"mode":"auto","allow_fallback":false}`,
			node:  `{"main":{"kind":"local"},"sub":{"kind":"local"}}`,
			want:  ClaudeRoutingPolicy{Main: ClaudeRouteWaired, Sub: ClaudeRouteSame},
		},
		{
			name:  "only route file present",
			route: `{"mode":"anthropic"}`,
			node:  "",
			want:  ClaudeRoutingPolicy{Main: ClaudeRouteAnthropic, Sub: ClaudeRouteSame},
		},
		{
			name:  "only node file present (anthropic main)",
			route: "",
			node:  `{"main":{"kind":"anthropic"},"sub":{"kind":"local"}}`,
			want:  ClaudeRoutingPolicy{Main: ClaudeRouteAnthropic, Sub: ClaudeRouteAuto},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			runtimeDir := filepath.Join(dir, "runtime")
			if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if c.route != "" {
				mustWrite(t, filepath.Join(runtimeDir, "desired-claude-route"), c.route)
			}
			if c.node != "" {
				mustWrite(t, filepath.Join(runtimeDir, "desired-claude-node"), c.node)
			}
			migrated, err := MigrateDesiredClaudeRouting(dir)
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if !migrated {
				t.Fatal("expected migrated=true")
			}
			got, err := ReadDesiredClaudeRouting(dir)
			if err != nil {
				t.Fatalf("read after migrate: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			// Legacy files are removed.
			if _, err := os.Stat(filepath.Join(runtimeDir, "desired-claude-route")); !os.IsNotExist(err) {
				t.Errorf("desired-claude-route not removed: %v", err)
			}
			if _, err := os.Stat(filepath.Join(runtimeDir, "desired-claude-node")); !os.IsNotExist(err) {
				t.Errorf("desired-claude-node not removed: %v", err)
			}
		})
	}
}

func TestMigrateDesiredClaudeRoutingNoLegacyFilesIsNoop(t *testing.T) {
	dir := t.TempDir()
	migrated, err := MigrateDesiredClaudeRouting(dir)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if migrated {
		t.Fatal("expected migrated=false when no legacy files")
	}
	if _, err := os.Stat(DesiredClaudeRoutingPath(dir)); !os.IsNotExist(err) {
		t.Fatal("no new file should be written for a fresh host")
	}
}

func TestMigrateDesiredClaudeRoutingSkipsWhenNewFileExists(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesiredClaudeRouting(dir, ClaudeRoutingPolicy{Main: ClaudeRouteWaired, Sub: ClaudeRouteSame}); err != nil {
		t.Fatal(err)
	}
	// A stray legacy file must NOT override the already-migrated new file.
	runtimeDir := filepath.Join(dir, "runtime")
	mustWrite(t, filepath.Join(runtimeDir, "desired-claude-route"), `{"mode":"anthropic"}`)
	migrated, err := MigrateDesiredClaudeRouting(dir)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if migrated {
		t.Fatal("expected migrated=false when new file already exists")
	}
	got, err := ReadDesiredClaudeRouting(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Main != ClaudeRouteWaired {
		t.Fatalf("existing policy overwritten: got %+v", got)
	}
}

func writeRaw(t *testing.T, stateDir, body string) {
	t.Helper()
	mustWrite(t, DesiredClaudeRoutingPath(stateDir), body)
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

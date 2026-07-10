package claudecode

import (
	"errors"
	"path/filepath"
)

// statusLine scope walk (#599).
//
// Claude Code's statusLine is single-slot with documented precedence
// managed-settings > <project>/.claude/settings.local.json >
// <project>/.claude/settings.json > ~/.claude/settings.json. waired installs
// its segment at the lowest-precedence (user) scope, so anything above it
// shadows the segment entirely — and DetectStatusLine alone (user file only)
// cannot see that. DetectEffectiveStatusLine walks the chain for a session
// rooted at cwd so enable/status can warn about shadowing instead of
// reporting an installed-but-invisible segment as success.

// StatusLineScope names the settings scope that defines the effective
// statusLine.
type StatusLineScope string

const (
	// ScopeNone: no statusLine in any visible scope.
	ScopeNone StatusLineScope = ""
	// ScopeManaged: system-wide managed-settings.json (highest precedence).
	ScopeManaged StatusLineScope = "managed"
	// ScopeProjectLocal: <project>/.claude/settings.local.json.
	ScopeProjectLocal StatusLineScope = "project-local"
	// ScopeProject: <project>/.claude/settings.json.
	ScopeProject StatusLineScope = "project"
	// ScopeUser: ~/.claude/settings.json — the scope waired installs into.
	ScopeUser StatusLineScope = "user"
)

// EffectiveStatusLine is the statusLine Claude Code would use for a session
// rooted at the probed cwd.
type EffectiveStatusLine struct {
	Scope   StatusLineScope
	Path    string // settings file defining it; empty when Scope == ScopeNone
	Kind    StatusLineKind
	Command string
}

// Shadowed reports whether a waired segment installed at the user scope
// would be invisible in sessions rooted at the probed cwd.
func (e EffectiveStatusLine) Shadowed() bool {
	return e.Scope == ScopeManaged || e.Scope == ScopeProjectLocal || e.Scope == ScopeProject
}

// DetectEffectiveStatusLine walks Claude Code's statusLine precedence for a
// session rooted at cwd: managedPath first, then the project chain found by
// walking up from cwd (at each level .claude/settings.local.json before
// .claude/settings.json; the nearest level with a statusLine wins), then the
// user-global file. home's own .claude directory is the user scope, not a
// project, and is skipped during the walk. Project/managed files that are
// missing, unreadable, or malformed are skipped best-effort — the walk exists
// to warn, never to block. cwd == "" skips the project chain. managedPath ==
// "" skips the managed check (callers pass claudemanaged.Path(); injectable
// so tests never read the real /etc file).
func DetectEffectiveStatusLine(home, cwd, managedPath string) (EffectiveStatusLine, error) {
	if home == "" {
		return EffectiveStatusLine{}, errors.New("claudecode: empty home")
	}
	if managedPath != "" {
		if eff, ok := statusLineAt(home, managedPath, ScopeManaged); ok {
			return eff, nil
		}
	}
	if cwd != "" {
		homeClean := filepath.Clean(home)
		for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
			if dir != homeClean { // ~/.claude is the user scope, not a project
				local := filepath.Join(dir, ".claude", "settings.local.json")
				if eff, ok := statusLineAt(home, local, ScopeProjectLocal); ok {
					return eff, nil
				}
				shared := filepath.Join(dir, ".claude", "settings.json")
				if eff, ok := statusLineAt(home, shared, ScopeProject); ok {
					return eff, nil
				}
			}
			if dir == filepath.Dir(dir) { // reached the filesystem root
				break
			}
		}
	}
	if eff, ok := statusLineAt(home, SettingsPath(home), ScopeUser); ok {
		return eff, nil
	}
	return EffectiveStatusLine{Scope: ScopeNone, Kind: StatusLineNone}, nil
}

// statusLineAt reads one settings file and reports its statusLine, if any.
// Read or parse failures report "no statusLine" — see the best-effort note on
// DetectEffectiveStatusLine.
func statusLineAt(home, path string, scope StatusLineScope) (EffectiveStatusLine, bool) {
	m, err := readSettings(path)
	if err != nil {
		return EffectiveStatusLine{}, false
	}
	if _, ok := m[statuslineKey]; !ok {
		return EffectiveStatusLine{}, false
	}
	kind, cmd := classifyStatusLine(home, m)
	if kind == StatusLineNone {
		return EffectiveStatusLine{}, false
	}
	return EffectiveStatusLine{Scope: scope, Path: path, Kind: kind, Command: cmd}, true
}

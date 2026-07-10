// Package shellalias hosts the sentinel constants and rc-file
// inventory shared between the installer (`waired link`) and the
// detector (`internal/integration/detect`). Keeping them in one place
// guarantees both halves agree on which files to touch / inspect and
// on how a "managed by waired" block is identified.
package shellalias

import (
	"path/filepath"
	"strings"
)

// Sentinel markers wrapping the `alias claude=...` (or fish function)
// block. Distinct from the legacy `# >>> waired managed` markers so
// the two coexist on the same machine during the v1 → v2 migration:
// `waired unlink` only removes what it installed.
const (
	SentinelOpen  = "# >>> waired-claude alias (do not edit) >>>"
	SentinelClose = "# <<< waired-claude alias <<<"
)

// RC is a candidate shell-rc file we may install into / detect inside.
type RC struct {
	Path string
	Fish bool
}

// RCCandidates returns the rc files we consider, in deterministic
// order. The caller filters to ones that actually exist on disk.
func RCCandidates(homeDir string) []RC {
	return []RC{
		{Path: filepath.Join(homeDir, ".bashrc")},
		{Path: filepath.Join(homeDir, ".zshrc")},
		{Path: filepath.Join(homeDir, ".config", "fish", "config.fish"), Fish: true},
	}
}

// FindBlock locates the sentinel-bracketed span in data. The returned
// (start, end) is suitable for slicing — end points past the closing
// sentinel line's trailing newline (when present) so callers can
// excise the block cleanly.
func FindBlock(data []byte) (start, end int, ok bool) {
	openIdx := strings.Index(string(data), SentinelOpen)
	if openIdx < 0 {
		return 0, 0, false
	}
	closeIdx := strings.Index(string(data[openIdx:]), SentinelClose)
	if closeIdx < 0 {
		return 0, 0, false
	}
	closeIdx += openIdx + len(SentinelClose)
	if closeIdx < len(data) && data[closeIdx] == '\n' {
		closeIdx++
	}
	return openIdx, closeIdx, true
}

// ExtractCommand pulls the shell command string out of a sentinel
// block. For POSIX rc files it reads `alias claude='...'`; for fish
// it reads the body of `function claude ... end`. Returns "" when
// nothing matches — callers treat that as "block present but
// unparseable" (typically a stale install from a prior format).
//
// The block argument should be the slice between FindBlock's
// (start, end). Surrounding context (other rc content) is ignored.
func ExtractCommand(block []byte, fish bool) string {
	for _, raw := range strings.Split(string(block), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if fish {
			if v, ok := extractFishFunction(line); ok {
				return v
			}
			continue
		}
		if v, ok := extractPosixAlias(line); ok {
			return v
		}
	}
	return ""
}

func extractPosixAlias(line string) (string, bool) {
	const prefix = "alias claude="
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	rhs := strings.TrimPrefix(line, prefix)
	return unquoteShellSingle(rhs), true
}

func extractFishFunction(line string) (string, bool) {
	if !strings.HasPrefix(line, "function claude") {
		return "", false
	}
	idx := strings.Index(line, ";")
	if idx < 0 {
		return "", false
	}
	body := strings.TrimSpace(line[idx+1:])
	body = strings.TrimSuffix(body, "end")
	body = strings.TrimSpace(body)
	body = strings.TrimSuffix(body, ";")
	body = strings.TrimSpace(body)
	body = strings.TrimSuffix(body, "$argv")
	body = strings.TrimSpace(body)
	if body == "" {
		return "", false
	}
	// Fish form is `<quoted-binary> claude` (binary may be quoted with
	// single quotes). Unquote just the binary token, leave the rest.
	if strings.HasPrefix(body, "'") {
		end := indexUnescapedSingleQuote(body[1:])
		if end < 0 {
			return body, true
		}
		bin := unquoteShellSingle(body[:end+2])
		return strings.TrimSpace(bin + " " + strings.TrimSpace(body[end+2:])), true
	}
	return body, true
}

// indexUnescapedSingleQuote returns the index of the first `'` in s
// that is not part of the POSIX `'\”` escape sequence. Returns -1
// if none is found. Caller passes s with the leading opening quote
// already stripped.
func indexUnescapedSingleQuote(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '\'' {
			continue
		}
		// Check for `'\''` continuation: current `'` followed by `\`
		// `'` `'` would appear at i, i+1, i+2, i+3 but in our slice
		// (leading `'` stripped) the pattern is `'\''` starting at i
		// when we're closing the run. Easier: peek if `\''` follows.
		if i+2 < len(s) && s[i+1] == '\\' && s[i+2] == '\'' {
			i += 2
			continue
		}
		return i
	}
	return -1
}

// unquoteShellSingle reverses the POSIX `'...'` quoting (with the
// `'\”` escape for embedded apostrophes) used by the installer's
// shellQuote. Anything outside that exact form is returned verbatim
// — robustness over strictness, since detection should not silently
// drop a value just because its quoting is unfamiliar.
func unquoteShellSingle(s string) string {
	if !strings.HasPrefix(s, "'") || !strings.HasSuffix(s, "'") || len(s) < 2 {
		return s
	}
	inner := s[1 : len(s)-1]
	return strings.ReplaceAll(inner, `'\''`, "'")
}

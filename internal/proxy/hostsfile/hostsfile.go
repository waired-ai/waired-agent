// Package hostsfile manages a marker-delimited block in the OS hosts file that
// redirects the intercepted hostnames (e.g. api.anthropic.com) to 127.0.0.1
// while the transparent proxy runs. Edits are crash-safe in the sense that Add
// reconciles (strips any stale block before re-adding) and Remove strips the
// block, so the redirect can always be cleaned up on the next start even if a
// previous run exited abruptly.
//
// The redirect is "sticky" system state: if it is left in place while nothing
// is listening on 127.0.0.1:443, every Claude Code request to api.anthropic.com
// fails with connection-refused. The production wiring therefore owns the block
// from systemd ExecStartPost/ExecStopPost (root) so it is removed even on crash;
// see internal/platform/service and cmd/waired `proxy hosts-{add,remove}`.
//
// The hosts path is injectable so tests never touch the real system file:
// pointing api.anthropic.com at 127.0.0.1 on the dev machine would break every
// live Claude Code session. The DefaultPath is platform-specific (path_*.go).
//
// Ported from the foxco PromptGate proxy (internal/hostsfile).
package hostsfile

import (
	"os"
	"strings"
)

const (
	beginMarker = "# >>> waired claude proxy >>>"
	endMarker   = "# <<< waired claude proxy <<<"
)

// Manager edits a single marker-delimited block in the hosts file at path,
// mapping each host to 127.0.0.1.
type Manager struct {
	path  string
	hosts []string
}

// New returns a Manager. An empty path defaults to the platform hosts file
// (DefaultPath).
func New(path string, hosts []string) *Manager {
	if path == "" {
		path = DefaultPath
	}
	return &Manager{path: path, hosts: hosts}
}

// Add reconciles the hosts file so it contains exactly one fresh redirect block,
// then flushes the OS resolver cache (Windows) so the redirect takes effect
// immediately.
func (m *Manager) Add() error {
	return withHostsLock(m.path, func() error {
		existing, err := m.read()
		if err != nil {
			return err
		}
		if err := m.write(render(existing, m.blockLines())); err != nil {
			return err
		}
		flushDNS()
		return nil
	})
}

// Remove strips the redirect block, leaving all other content intact, then
// flushes the OS resolver cache (Windows). A missing file or absent block is a
// no-op (and skips the flush, since nothing changed).
func (m *Manager) Remove() error {
	return withHostsLock(m.path, func() error {
		existing, err := m.read()
		if err != nil {
			return err
		}
		if !strings.Contains(existing, beginMarker) {
			return nil
		}
		if err := m.write(render(existing, nil)); err != nil {
			return err
		}
		flushDNS()
		return nil
	})
}

// Present reports whether the redirect block is currently in the file.
func (m *Manager) Present() (bool, error) {
	existing, err := m.read()
	if err != nil {
		return false, err
	}
	return strings.Contains(existing, beginMarker), nil
}

func (m *Manager) read() (string, error) {
	b, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(b), nil
}

// write rewrites the file in place (O_TRUNC, via os.WriteFile) to preserve its
// owner/permissions/ACL, rather than replacing it via rename (which would drop
// them).
func (m *Manager) write(content string) error {
	return os.WriteFile(m.path, []byte(content), 0o644)
}

func (m *Manager) blockLines() []string {
	lines := make([]string, 0, len(m.hosts)+2)
	lines = append(lines, beginMarker)
	for _, h := range m.hosts {
		lines = append(lines, "127.0.0.1 "+h)
	}
	lines = append(lines, endMarker)
	return lines
}

// render returns existing content with any prior redirect block stripped, then
// (if blockLines is non-nil) the fresh block appended. Surrounding content and
// the file's dominant EOL style are preserved.
func render(existing string, blockLines []string) string {
	lines, eol := splitLines(existing)
	lines = stripBlock(lines)
	// Trim trailing blank lines so we don't accumulate them across edits.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}

	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(ln)
		b.WriteString(eol)
	}
	if len(blockLines) > 0 {
		if b.Len() > 0 {
			b.WriteString(eol) // blank separator before our block
		}
		for _, ln := range blockLines {
			b.WriteString(ln)
			b.WriteString(eol)
		}
	}
	return b.String()
}

func splitLines(s string) (lines []string, eol string) {
	eol = "\n"
	if strings.Contains(s, "\r\n") {
		eol = "\r\n"
	}
	if s == "" {
		return nil, eol
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	parts := strings.Split(s, "\n")
	// A trailing newline yields a final "" element; drop it.
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts, eol
}

// stripBlock removes all lines from the begin marker through the end marker
// (inclusive). A begin marker with no end marker strips to end-of-file.
func stripBlock(lines []string) []string {
	out := make([]string, 0, len(lines))
	inBlock := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		switch {
		case t == beginMarker:
			inBlock = true
		case t == endMarker:
			inBlock = false
		case !inBlock:
			out = append(out, ln)
		}
	}
	return out
}

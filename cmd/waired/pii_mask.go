package main

import (
	"io"
	"os"
	"os/user"
	"regexp"
	"strings"
	"sync"
)

// PII masking for screenshots / bug reports (--mask-pii / WAIRED_PII_MASK):
// `waired init` output — including everything child installers print —
// has the invoking user's home directory, username, hostname and account
// email replaced with placeholder tokens. Best-effort by design: it exists
// so a user can paste a first-run transcript into an issue without hand
// redacting it, not as a security boundary. Daemon logs are out of scope.

// piiMasker holds the ordered replacement set. Literal replacements run
// longest-first (a home dir contains the username, so it must go first);
// the email regex runs last.
type piiMasker struct {
	literals []struct{ from, to string }
	patterns []struct {
		re *regexp.Regexp
		to string
	}
}

var emailRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)

// newPIIMasker builds the replacement set from the current process
// environment. Every probe is best-effort — a lookup failure just means
// that token is not masked.
func newPIIMasker() *piiMasker {
	m := &piiMasker{}
	addLiteral := func(from, to string) {
		// Guard against tiny/absent tokens: masking "" or a 1-2 char
		// username would mangle unrelated output.
		if len(from) < 3 {
			return
		}
		m.literals = append(m.literals, struct{ from, to string }{from, to})
	}

	if home, err := os.UserHomeDir(); err == nil {
		addLiteral(home, "<home>")
	}
	// The email regex must run BEFORE the username patterns: an account
	// email usually contains the username, and masking it first keeps
	// "alice@example.com" as one clean <email> instead of "<user>@...".
	m.patterns = append(m.patterns, struct {
		re *regexp.Regexp
		to string
	}{emailRe, "<email>"})

	names := make([]string, 0, 2)
	if u, err := user.Current(); err == nil && u.Username != "" {
		name := u.Username
		// Windows reports DOMAIN\name; mask the bare name.
		if i := strings.LastIndexAny(name, `\/`); i >= 0 {
			name = name[i+1:]
		}
		names = append(names, name)
	}
	if su := os.Getenv("SUDO_USER"); su != "" {
		names = append(names, su)
		// Under sudo, HOME is root's — mask the invoking user's real home
		// too (init prints per-user integration paths there).
		if lu, err := user.Lookup(su); err == nil {
			addLiteral(lu.HomeDir, "<home>")
		}
	}
	for _, n := range names {
		if len(n) < 3 {
			continue
		}
		m.patterns = append(m.patterns, struct {
			re *regexp.Regexp
			to string
		}{regexp.MustCompile(`\b` + regexp.QuoteMeta(n) + `\b`), "<user>"})
	}
	if host, err := os.Hostname(); err == nil {
		addLiteral(host, "<host>")
	}
	return m
}

func (m *piiMasker) mask(s string) string {
	for _, l := range m.literals {
		s = strings.ReplaceAll(s, l.from, l.to)
	}
	for _, p := range m.patterns {
		s = p.re.ReplaceAllString(s, p.to)
	}
	return s
}

// enablePIIMask swaps os.Stdout / os.Stderr for pipes whose reader side
// masks and forwards to the real streams, so EVERY print in this process —
// and in child processes that inherit the fds (the engine installer, the
// embedded ollama-windows.ps1) — is masked without touching call sites.
//
// Deliberate consequence: os.Stdout stops being a terminal, so isTerminal
// gates fall back to their non-TTY rendering (fresh sparse progress lines
// instead of `\r` rewrites). For the masked transcript use case that is the
// BETTER output — no overwritten control-character soup in the paste.
//
// Masking is applied per read chunk. fmt's writes are single syscalls well
// under the pipe buffer, so a token split across chunks is possible only
// for exotic writers — an accepted limit of a best-effort feature.
func enablePIIMask() (restore func()) {
	m := newPIIMasker()

	swap := func(orig *os.File, set func(*os.File)) (cleanup func()) {
		r, w, err := os.Pipe()
		if err != nil {
			return func() {}
		}
		set(w)
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 32*1024)
			for {
				n, rerr := r.Read(buf)
				if n > 0 {
					_, _ = io.WriteString(orig, m.mask(string(buf[:n])))
				}
				if rerr != nil {
					return
				}
			}
		}()
		return func() {
			_ = w.Close()
			wg.Wait()
			_ = r.Close()
			set(orig)
		}
	}

	outClean := swap(os.Stdout, func(f *os.File) { os.Stdout = f })
	errClean := swap(os.Stderr, func(f *os.File) { os.Stderr = f })
	return func() {
		outClean()
		errClean()
	}
}

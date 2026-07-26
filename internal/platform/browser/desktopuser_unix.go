//go:build linux || darwin

package browser

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// hopTimeout bounds the de-escalated launch. open(1) and xdg-open both hand
// the URL to the handler and exit immediately, so this is only ever reached
// when something is wedged. A var so the timeout path is testable in
// milliseconds rather than in tens of seconds.
var hopTimeout = 10 * time.Second

// lookupTimeout bounds the uid lookups below.
const lookupTimeout = 5 * time.Second

// lookupUID resolves a username to its numeric uid.
//
// The `id -u` fallback is required, not defensive: the darwin agent and CLI
// are built with CGO_ENABLED=0 (Makefile: build-agent-darwin), where
// user.Lookup reads /etc/passwd only — and macOS keeps real accounts in
// OpenDirectory, so the pure-Go lookup misses every human user. It is the same
// story for NSS/LDAP users on Linux. Returns "" when the uid cannot be
// resolved; callers degrade rather than fail.
func lookupUID(name string) string {
	if u, err := user.Lookup(name); err == nil && u.Uid != "" {
		return u.Uid
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/id", "-u", name).Output()
	if err != nil {
		return ""
	}
	uid := strings.TrimSpace(string(out))
	if n, err := strconv.Atoi(uid); err != nil || n <= 0 {
		return ""
	}
	return uid
}

// runHop runs a de-escalated launcher argv.
//
// stdin is deliberately left nil (/dev/null): the CLI's sign-in flow reads
// Enter from the terminal around this call, and a child inheriting stdin would
// race it for the keystroke.
//
// A non-zero exit or a failure to spawn is returned so Open can fall back to
// the direct launch — degrading to today's behaviour is always better than not
// opening anything. A timeout is NOT an error: the handler was most likely
// launched and only the wrapper is lingering, and falling back there would
// open a second browser window.
func runHop(argv []string) error {
	if len(argv) == 0 {
		return errors.New("browser: empty hop argv")
	}
	ctx, cancel := context.WithTimeout(context.Background(), hopTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	cmd.Env = scrubEnv(os.Environ())
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

package tray

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"

	"github.com/waired-ai/waired-agent/internal/platform/paths"
)

// The state dir the app hands to an elevated action, and how it is decided.
//
// The app never touches the state dir itself — it runs as the desktop user and
// talks only to the Local Management API. But three of its actions shell out to
// an ELEVATED `waired`, and that process is told which directory to work on:
// sign-out wipes it, the engine install writes into it, and the legacy sign-in
// fallback enrolls into it. Naming the wrong one is silent in all three: the
// elevated CLI succeeds, on the wrong directory, and exits 0.
//
// That is waired-agent#1269. The old answer stat'd
// `<system state dir>/identity.json` and read ANY error as "there is no system
// install". The system state dir is 0700 root on both Unixes and DACL'd to
// SYSTEM+Administrators on Windows (internal/platform/secrets), so a
// desktop-user process always gets EACCES and the app always fell through to
// the per-user directory — on every OS, on every system install. The elevated
// sign-out then wiped an empty directory, printed
// "logout: identity + secrets removed." and returned 0.
//
// The same misreading was already found and fixed twice elsewhere in this
// repo: cmd/waired/statehint.go's resolveSystemFallbackAt ("the honest
// replacement for the old os.Stat guess") and cmd/waired/doctor_statedir.go's
// absent / unreadable / system-wide split (waired-agent#1005). The installer
// records it too (packaging/install/install.sh: "a bare `[ -e ]` answers 'not
// enrolled' on a host that is"). Only the app's copy was left behind.

// systemDirAnswer is what a look at the system state dir established. The
// point of the type is that "absent" and "unreadable" are different answers;
// collapsing them is the defect.
type systemDirAnswer int

const (
	// systemDirAbsent: there is no system-wide install to point at.
	systemDirAbsent systemDirAnswer = iota
	// systemDirEnrolled: a system-wide install this process can see. Reached
	// by an elevated or developer run, not by the shipped desktop app.
	systemDirEnrolled
	// systemDirUnreadable: a system-wide install this process is locked out
	// of. THE waired-agent#1269 CASE, and the ordinary state of the shipped
	// app on every OS.
	systemDirUnreadable
)

func (a systemDirAnswer) String() string {
	switch a {
	case systemDirEnrolled:
		return "system-enrolled"
	case systemDirUnreadable:
		return "system-unreadable"
	default:
		return "system-absent"
	}
}

// stateDirSource names which rule produced the answer, so a test can prove a
// row landed on the right directory for the right reason and the debug line
// can say why.
type stateDirSource int

const (
	sourceOverride stateDirSource = iota // --state-dir / $WAIRED_STATE_DIR
	sourceDaemon                         // the running daemon declared it
	sourceSystem                         // the system-wide install
	sourceUser                           // this user's own state dir
)

func (s stateDirSource) String() string {
	switch s {
	case sourceOverride:
		return "override"
	case sourceDaemon:
		return "daemon"
	case sourceSystem:
		return "system"
	default:
		return "user"
	}
}

// stateDirFacts is everything the decision is made from. Gathering it touches
// the filesystem and the daemon; deciding from it does not.
type stateDirFacts struct {
	// Override is a state dir the operator named explicitly. Wins outright.
	Override string
	// DaemonDir is `state_dir` from GET /waired/v1/setup/state — the daemon's
	// own answer about its own directory. Empty when the daemon did not say
	// (no live session, an older daemon, or it could not be reached).
	DaemonDir string
	// SystemDir / UserDir are the platform's two candidates.
	SystemDir string
	UserDir   string
	// System is what a look at SystemDir established.
	System systemDirAnswer
}

// classifySystemDir turns the result of listing the system state dir into an
// answer.
//
// It takes a ReadDir result rather than a stat of identity.json for two
// reasons. The app's contract with itself is that it "never reads
// identity.json directly so it stays safely outside the daemon's privilege
// boundary" (cmd/waired-tray/main.go) — listing names keeps that true. And a
// directory that cannot be opened is exactly the signal we need, which is what
// the old code threw away.
//
// Go maps Windows ERROR_ACCESS_DENIED onto fs.ErrPermission, so the one
// permission branch covers all three OSes — the same reasoning
// cmd/waired/statehint.go already records for the CLI.
func classifySystemDir(names []string, err error) systemDirAnswer {
	switch {
	case err == nil:
		for _, n := range names {
			if n == "identity.json" {
				return systemDirEnrolled
			}
		}
		// Readable and holding no identity: a directory the installer laid
		// down on a host nobody has signed in on yet. Not a system install to
		// point an elevated action at.
		return systemDirAbsent
	case errors.Is(err, fs.ErrPermission):
		return systemDirUnreadable
	default:
		// ErrNotExist, and anything else. Failing to "absent" keeps an
		// unexpected error on today's answer rather than inventing a new
		// failure mode — what resolveSystemFallbackAt's default arm does for
		// the same question.
		return systemDirAbsent
	}
}

// resolveStateDir picks the directory an elevated action should be told to
// work on.
//
// Deliberately NOT parameterised on GOOS: nothing here branches on the
// operating system. What varies per OS is the two candidate PATHS, and
// internal/platform/paths owns those together with its own tests. The table
// test still names its rows per OS so all three are visible at once.
func resolveStateDir(f stateDirFacts) (string, stateDirSource) {
	if f.Override != "" {
		return f.Override, sourceOverride
	}
	// The daemon is the authority on its own directory, for the reason its own
	// field documents (internal/management/setup_handlers.go): a client-side
	// default silently diverges from a daemon started with --state-dir or
	// $WAIRED_STATE_DIR, and the symptom of divergence is silent. Empty means
	// it did not say — never "there is nothing there".
	if f.DaemonDir != "" {
		return f.DaemonDir, sourceDaemon
	}
	if f.System == systemDirEnrolled || f.System == systemDirUnreadable {
		return f.SystemDir, sourceSystem
	}
	return f.UserDir, sourceUser
}

// readStateDirNames lists a state dir. A seam because the permission arm is
// what matters here and it cannot be staged portably: os.Chmod on Windows
// toggles the read-only attribute rather than denying traversal, so a
// chmod-based test there produces ENOENT and exercises the wrong branch — the
// same reason cmd/waired-agent/login.go seams its own os.Stat.
var readStateDirNames = osReadStateDirNames

func osReadStateDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

// localStateDirFacts gathers everything except the daemon's answer.
func localStateDirFacts() stateDirFacts {
	sys := paths.StateDir(paths.System)
	f := stateDirFacts{
		Override:  os.Getenv(paths.EnvOverride),
		SystemDir: sys,
		UserDir:   paths.StateDir(paths.Interactive),
	}
	f.System = classifySystemDir(readStateDirNames(sys))
	return f
}

// DefaultStateDir is the app's answer before it has spoken to a daemon. It is
// the flag default in cmd/waired-tray, so `waired-tray -h` prints it — which
// is also the cheapest way to see waired-agent#1269 on a real host, with no
// debugger attached.
func DefaultStateDir() string {
	dir, _ := resolveStateDir(localStateDirFacts())
	return dir
}

// elevationStateDir is the directory to hand an elevated action, decided at
// the moment of the action rather than at startup.
//
// Per action, because the daemon is often not up when the app starts — Windows
// registers the service delayed-auto-start, and on macOS the login sequence
// races — so a startup answer would freeze the fallback for the life of the
// process. An elevated action is about to raise an authorization prompt
// anyway, so one short read costs nothing next to it.
//
// An operator who passed --state-dir gets exactly that, unasked: the same rule
// --log-level already follows (pinned by a flag, otherwise follow the daemon).
func (t *tray) elevationStateDir(ctx context.Context) string {
	if t.opts.StateDirPinned {
		return t.opts.StateDir
	}
	f := localStateDirFacts()
	if st, err := t.cli.SetupState(ctx); err == nil && st != nil {
		f.DaemonDir = st.StateDir
	}
	dir, src := resolveStateDir(f)
	slog.Debug("tray: state dir for an elevated action",
		"dir", dir, "source", src.String(), "system", f.System.String())
	return dir
}

// Package logrotate keeps a process's own launchd log files bounded
// without losing a line to the rotation.
//
// Why this exists (#331): on macOS the agent and the tray write stderr
// through a file descriptor launchd opened from the plist's
// StandardErrorPath. An external rotator — newsyslog(8), which the
// installer used to configure — renames that file and creates a fresh
// one, but the process keeps writing through the descriptor it already
// holds, which still points at the renamed (and, once gzipped, deleted)
// inode. Every line after the rotation is lost until the daemon
// restarts. On a host wedged in engine_failed for 12 hours, no restart
// ever comes, and the logs that would explain the wedge are exactly the
// ones that disappear.
//
// The fix is to rotate from inside the process that holds the
// descriptor, so the rename and the re-point are one operation:
//
//	shift archives -> rename live to <path>.0 -> reopen fd onto <path> -> gzip <path>.0
//
// The reopen (dup2 on the same fd number) happens before the
// compression, so the only writes that land in the rotated-away file are
// the ones from the microseconds between the rename and the dup2 — and
// those are kept, because that file becomes the .0.gz archive rather
// than being deleted. Nothing is dropped.
//
// Linux and Windows have no descriptor to re-point: systemd captures the
// unit's stdout/stderr into journald, and under the Windows SCM stderr is
// closed. The per-OS bodies say so and return a zero ops, which makes
// Manage a no-op there.
//
// Windows instead has the opposite problem — no stream at all, since the
// Event Log takes Warn and above only — so the agent opens and rotates a
// log file of its own. That is File (file.go): the same archives, the same
// policy, but the writer owns the handle rather than borrowing a
// descriptor from a service manager.
package logrotate

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"
)

// The launchd log paths. These constants are the single definition:
// internal/platform/service (the LaunchDaemon plist) and
// internal/platform/autostart (the tray LaunchAgent plist) write them
// into the plists, and this package rotates them. Keeping one source
// means the rotator can never end up watching a path the plist stopped
// using.
const (
	// AgentOutPath / AgentErrPath are the system LaunchDaemon's
	// StandardOutPath / StandardErrorPath.
	AgentOutPath = "/Library/Logs/waired-agent.out.log"
	AgentErrPath = "/Library/Logs/waired-agent.err.log"

	// trayOutName / trayErrName are the per-user tray LaunchAgent's log
	// file names under <home>/Library/Logs (see TrayOutPath).
	trayOutName = "waired-tray.out.log"
	trayErrName = "waired-tray.err.log"
)

// TrayOutPath / TrayErrPath return the tray LaunchAgent's
// StandardOutPath / StandardErrorPath for a given home directory. The
// tray is a per-user agent, so unlike the daemon its log paths are not
// constants.
//
// Joined with a literal "/" rather than filepath.Join: these are macOS
// paths whatever host builds or runs the code, and filepath.Join would
// make them depend on the *running* OS's separator — a darwin path
// spelled with backslashes when the same pure function is called from a
// Windows build. That is the shape of divergence the cross-OS parity
// rule exists to prevent, and it is why these functions are testable
// identically on all three OSes.
func TrayOutPath(home string) string { return trayLogPath(home, trayOutName) }
func TrayErrPath(home string) string { return trayLogPath(home, trayErrName) }

func trayLogPath(home, name string) string {
	return strings.TrimSuffix(home, "/") + "/Library/Logs/" + name
}

// checkEvery is how often Manage re-examines each target's size.
//
// Polling rather than counting bytes as they are written: this package
// does not own every writer of fd 1/2. slog goes through them, so do
// direct fmt.Fprintln(os.Stderr) calls, the Go runtime's panic output,
// and any child process that inherited them. A counting io.Writer
// wrapper would see only the first of those and undercount the file it
// is meant to bound.
const checkEvery = 60 * time.Second

// stagedSuffix names the rotated-away file between the rename and the
// gzip. It matches newsyslog's transient name for the same step, so the
// uninstaller's archive globs cover it too.
const stagedSuffix = ".0"

// Target is one log file plus the descriptor this process writes it
// through. FD is a raw descriptor number (1 for stdout, 2 for stderr)
// because that is what launchd handed us and what dup2 re-points.
type Target struct {
	Path string
	FD   int
}

// Policy is the size bound. The defaults reproduce what the retired
// newsyslog drop-in provided: rotate at 1 MB, keep 5 gzip'd archives.
type Policy struct {
	MaxBytes int64
	Keep     int
}

// DefaultPolicy is the policy that applies at info level and above.
func DefaultPolicy() Policy { return Policy{MaxBytes: 1 << 20, Keep: 5} }

// debugPolicy is what applies while the log level is debug. Bigger,
// because debug is roughly an order of magnitude more output and the
// default bound turns the standard bug-report advice ("raise verbosity,
// reproduce, then collect") against itself: on the rc8 macOS host at
// debug the file rotated every ~18 minutes, so five generations held
// about 90 minutes and an investigation into something an hour old found
// its evidence already gone (#658).
//
// 8 MB x 10 is roughly a day at the ~3.3 MB/h that host measured, for
// about 15 MB on disk — the live file plus nine archives, which gzip to
// well under a tenth of their size for JSON records.
func debugPolicy() Policy { return Policy{MaxBytes: 8 << 20, Keep: 10} }

// PolicyForLevel returns the bound to apply at lvl. Split from Policy
// itself so it is a pure function of the level, table-testable without a
// filesystem, and so callers can re-read it: the management API flips the
// live level without a restart (`waired config log-level debug`), and a
// rotation policy chosen once at boot would keep the old bound for the
// rest of the process's life.
func PolicyForLevel(lvl slog.Level) Policy {
	if lvl <= slog.LevelDebug {
		return debugPolicy()
	}
	return DefaultPolicy()
}

// AgentTargets returns the daemon's rotatable log files on goos, in the
// (GOOS, facts) -> plan shape the repo's cross-OS parity rule asks for.
// Only darwin has any: on Linux the systemd journal owns and bounds the
// unit's stdout/stderr, and on Windows the service writes to the
// Application Event Log (stderr is closed under the SCM).
func AgentTargets(goos string) []Target {
	if goos != "darwin" {
		return nil
	}
	return []Target{
		{Path: AgentOutPath, FD: 1},
		{Path: AgentErrPath, FD: 2},
	}
}

// TrayTargets returns the tray's rotatable log files on goos. Same
// reasoning as AgentTargets, plus: the tray's paths are relative to the
// invoking user's home, so an empty home yields no targets rather than
// a guess at "/Library/Logs".
func TrayTargets(goos, home string) []Target {
	if goos != "darwin" || home == "" {
		return nil
	}
	return []Target{
		{Path: TrayOutPath(home), FD: 1},
		{Path: TrayErrPath(home), FD: 2},
	}
}

// ops are the two OS-touching operations rotation needs. They are a
// parameter rather than a package-level var so the portable mechanics
// below can be exercised on any OS by a fake that records the arguments
// it was handed, while the real darwin implementations get their own
// test (logrotate_darwin_test.go) that runs them for real.
type ops struct {
	// reopen points fd at path's current inode, creating the file if
	// needed. Nil on an OS where this package does nothing.
	reopen func(path string, fd int) error
	// sameFile reports whether fd currently refers to the file at path.
	sameFile func(fd int, path string) (bool, error)
}

// Manage starts a goroutine that keeps targets within the policy, until
// ctx is done. It is a no-op when there is nothing to do: no targets, or
// an OS whose service manager already bounds the stream.
//
// policy is a function, not a value, because the level it derives from is
// live: `waired config log-level debug` changes the verbosity of a
// running daemon, and the bound has to follow it. Each sweep asks again.
//
// Every failure is logged and swallowed. A daemon must not fail to run
// because its log file could not be rotated.
func Manage(ctx context.Context, targets []Target, policy func() Policy, logger *slog.Logger) {
	if len(targets) == 0 || policy == nil {
		return
	}
	o := defaultOps()
	if o.reopen == nil || o.sameFile == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	go func() {
		t := time.NewTicker(checkEvery)
		defer t.Stop()
		for {
			sweep(targets, policy(), o, logger)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// sweep runs one pass over every target: finish any rotation a previous
// process died in the middle of, then rotate what is over the cap.
func sweep(targets []Target, p Policy, o ops, logger *slog.Logger) {
	for _, t := range targets {
		if err := recoverStaged(t.Path, p); err != nil {
			logger.Warn("could not compress a staged log archive",
				"path", t.Path+stagedSuffix, "err", err)
		}
		rotated, err := rotate(t, p, o)
		if err != nil {
			logger.Warn("log rotation failed", "path", t.Path, "err", err)
			continue
		}
		if rotated {
			logger.Info("rotated the log file", "path", t.Path, "keep", p.Keep)
		}
	}
}

// rotate rotates t when it is over p.MaxBytes, reporting whether it did.
//
// The order of the steps is the point of this package — see the package
// comment. It is also why the reopen error is fatal to the rotation: if
// the descriptor could not be re-pointed we have already renamed the
// live file, so the caller must know that this process's log stream is
// now going somewhere unexpected.
func rotate(t Target, p Policy, o ops) (bool, error) {
	fi, err := os.Stat(t.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if fi.Size() <= p.MaxBytes {
		return false, nil
	}

	// Only rotate a file this process is actually writing through t.FD.
	// Run from a terminal — a developer's foreground `waired-agent`, a
	// hand-started tray — fd 2 is the tty and this path is somebody
	// else's file (or a stale one), so renaming it would be both
	// useless and rude. It also means a wrong guess at the tray's home
	// directory fails safe.
	ours, err := o.sameFile(t.FD, t.Path)
	if err != nil {
		return false, err
	}
	if !ours {
		return false, nil
	}

	if err := shiftArchives(t.Path, p.Keep); err != nil {
		return false, err
	}
	staged := t.Path + stagedSuffix
	if err := os.Rename(t.Path, staged); err != nil {
		return false, err
	}
	// Before the gzip, not after: writes are still landing in `staged`
	// until this returns, and `staged` is kept as the .0.gz archive.
	// Compressing first would leave a window in which the process
	// writes into a file that is about to be deleted — the #331 bug,
	// reproduced from inside.
	if err := o.reopen(t.Path, t.FD); err != nil {
		return false, fmt.Errorf("reopen %s on fd %d: %w", t.Path, t.FD, err)
	}
	if err := compress(staged, archiveName(t.Path, 0)); err != nil {
		return true, err
	}
	return true, nil
}

// recoverStaged finishes a rotation that was interrupted between the
// rename and the gzip (a crash, a kill -9). The staged file holds real
// log data, so it is compressed rather than dropped.
//
// In the case this actually recovers, slot 0 is free — shiftArchives
// ran just before the rename that produced the staged file — so the
// shift here is a no-op and the archive order is exact. If some later
// rotation did fill slot 0 first, the shift keeps both files at the cost
// of ordering the recovered one as if it were the newest. Losing the
// ordering of a post-crash leftover beats losing the leftover.
func recoverStaged(path string, p Policy) error {
	staged := path + stagedSuffix
	if _, err := os.Stat(staged); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := shiftArchives(path, p.Keep); err != nil {
		return err
	}
	return compress(staged, archiveName(path, 0))
}

// archiveName is the newsyslog-compatible name of archive slot n:
// <path>.<n>.gz. Keeping the shape means the uninstaller's existing
// globs, the docs, and anyone's muscle memory all still work.
func archiveName(path string, n int) string { return fmt.Sprintf("%s.%d.gz", path, n) }

// shiftArchives ages every archive by one slot and drops what falls off
// the end, leaving slot 0 free. Missing slots are normal (a host that
// has rotated twice has two).
func shiftArchives(path string, keep int) error {
	for i := keep - 1; i >= 0; i-- {
		from := archiveName(path, i)
		if i+1 >= keep {
			if err := removeIfExists(from); err != nil {
				return err
			}
			continue
		}
		if err := os.Rename(from, archiveName(path, i+1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// compress gzips src to dst and removes src. A failure part-way leaves
// no half-written archive behind, so the next pass can retry from the
// still-present src.
//
// Both handles are closed explicitly before src is removed rather than
// on a defer. Unlinking a file that is still open is fine on Unix and an
// error on Windows ("being used by another process"), and this function
// is untagged: the per-OS bodies decide whether rotation runs, not which
// file semantics this code may assume.
func compress(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		in.Close()
		return err
	}
	zw := gzip.NewWriter(out)
	failure := func() error {
		if _, err := io.Copy(zw, in); err != nil {
			return err
		}
		return zw.Close()
	}()
	if err := out.Close(); err != nil && failure == nil {
		failure = err
	}
	if err := in.Close(); err != nil && failure == nil {
		failure = err
	}
	if failure != nil {
		_ = os.Remove(dst)
		return failure
	}
	return os.Remove(src)
}

package logrotate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The agent's own log file, for the OS where the service manager gives the
// process nowhere else to put it. <stateDir>\logs\waired-agent.log sits
// beside the bundled engine's <stateDir>\runtimes\<engine>\logs\engine.log,
// so one place holds everything a bug report needs.
const (
	agentLogDirName  = "logs"
	agentLogFileName = "waired-agent.log"
)

// AgentOwnedLogFile returns the log file the agent process opens and
// rotates itself on goos, or "" where it should not open one — in the
// (GOOS, facts) -> plan shape the repo's cross-OS parity rule asks for.
//
// Only Windows has one. Under the SCM stderr is closed, so the JSON
// handler's records go nowhere, and internal/platform/logsink mirrors only
// Warn and above to the Application Event Log — every INFO and DEBUG
// record is dropped (#636). Linux hands the unit's stderr to journald and
// macOS hands it to launchd's StandardErrorPath, both of which already
// hold the whole stream; opening a second file there would only duplicate
// it.
//
// An empty stateDir yields "" rather than a guessed path, the same way
// TrayTargets refuses to guess a home directory.
func AgentOwnedLogFile(goos, stateDir string) string {
	if goos != "windows" || stateDir == "" {
		return ""
	}
	return windowsLogPath(stateDir)
}

// AgentLogPath returns where an operator READS the agent's own log on
// goos, which is a different question from AgentOwnedLogFile: on macOS the
// process does not own the file — launchd opened it — but it is still the
// file to read. Linux yields "" because there is no file at all; the
// journal is the source (`journalctl -u waired-agent`).
//
// This is the one definition for every surface that names the path —
// `waired logs`, the tray's diagnostic hint, `waired doctor` — so none of
// them can drift from where the log actually is.
func AgentLogPath(goos, stateDir string) string {
	switch goos {
	case "windows":
		return AgentOwnedLogFile(goos, stateDir)
	case "darwin":
		return AgentErrPath
	default:
		return ""
	}
}

// windowsLogPath joins the Windows path with a literal separator rather
// than filepath.Join, for the reason spelled out at TrayOutPath: Join
// depends on the *running* OS's separator, which would spell a Windows
// path with forward slashes whenever this pure function is called from a
// Linux or macOS build — including from its own table test. A stateDir
// that arrived with forward slashes (a hand-set $WAIRED_STATE_DIR) is
// normalized so the result is not a mix of both.
func windowsLogPath(stateDir string) string {
	dir := strings.ReplaceAll(stateDir, "/", `\`)
	dir = strings.TrimSuffix(dir, `\`)
	return dir + `\` + agentLogDirName + `\` + agentLogFileName
}

// File is a size-bounded log file that this process opens, writes, and
// rotates itself. It is the counterpart to the descriptor-based rotation
// above, for the case where no service manager handed us a stream: there
// is no foreign fd to re-point, so the writer owns the handle outright.
//
// Rotation produces the same <path>.<n>.gz archives as the descriptor
// path, which is what lets internal/platform/logdump collect both with one
// glob and keeps the uninstaller's existing archive patterns working.
//
// Safe for concurrent use: slog writes one record per Write from any
// goroutine.
type File struct {
	path string
	// policy is asked again on every write rather than captured once:
	// the log level it derives from is live (see PolicyForLevel), so a
	// daemon switched to debug must start keeping the larger window
	// immediately, not after a restart.
	policy func() Policy

	mu     sync.Mutex
	file   *os.File
	size   int64
	closed bool
	// noted records that the current run of rotation failures has already
	// been written into the log, so a lasting failure produces one note
	// rather than one per record. Cleared by the next rotation that works.
	noted bool
}

// OpenFile opens path for appending, creating it and its parent directory
// if needed, and returns a writer that keeps it within whatever policy
// reports at the time of each write.
//
// The mode is 0o600: the agent's log carries local paths, device names and
// (at debug) mesh detail, so it should not be world-readable where the
// filesystem has an opinion. On Windows the ACL comes from the state dir
// instead, which the service installer locks to SYSTEM + Administrators
// (internal/platform/secrets.SecureDir) — reading it there needs an
// elevated shell, exactly as the bundled engine's log already does.
func OpenFile(path string, policy func() Policy) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Fold in anything a previous process was killed part-way through
	// rotating, BEFORE the first write: recoverStaged puts <path>.0 into
	// archive slot 0, and doing it now means a later rotation's rename
	// cannot land on top of it. A failure here is deliberately not fatal —
	// the leftover stays on disk and rotate() retries — because the daemon
	// having somewhere to log matters more than the archive being tidy.
	_ = recoverStaged(path, policy())

	f := &File{path: path, policy: policy}
	if err := f.open(); err != nil {
		return nil, err
	}
	return f, nil
}

// Write appends b, rotating first when b would carry the file past the
// cap. The record itself is never split: a single record larger than
// MaxBytes is written whole and rotated away by the next one.
func (f *File) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return 0, fs.ErrClosed
	}
	// Reopen after a failed rotation left us without a handle, so a
	// transient error (a directory momentarily gone, a full disk that
	// cleared) does not silence the log for the rest of the process's life.
	if f.file == nil {
		if err := f.open(); err != nil {
			return 0, err
		}
	}
	// f.size > 0 keeps an empty file from rotating: without it an
	// oversized record would archive nothing and then land in the fresh
	// file anyway.
	var rotErr error
	if p := f.policy(); f.size > 0 && f.size+int64(len(b)) > p.MaxBytes {
		rotErr = f.rotate(p)
	}
	// A rotation that failed part-way may have left no handle behind. Get
	// one before touching it — both the note below and the record itself
	// need somewhere to go.
	if f.file == nil {
		if err := f.open(); err != nil {
			return 0, err
		}
	}
	// Note the failure once per episode, not once per record. A rotation
	// that fails for a lasting reason — a full disk, an ACL that changed
	// under us — leaves f.size over the cap, so every subsequent Write
	// retries and fails the same way. Without this the file would carry one
	// extra WARN for every record it holds, which is the opposite of
	// bounding it.
	if rotErr != nil && !f.noted {
		f.noted = true
		f.size += f.noteRotationFailure(rotErr)
	}
	n, err := f.file.Write(b)
	f.size += int64(n)
	return n, err
}

// Close releases the handle. Writes afterwards fail rather than reopening.
// Closing twice is not an error, so a deferred Close beside an explicit one
// is harmless.
func (f *File) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if f.file == nil {
		return nil
	}
	err := f.file.Close()
	f.file = nil
	return err
}

// open attaches a handle to f.path and records the size already on disk.
// Reading the size back rather than starting from zero is what makes the
// cap survive a restart: a daemon that comes up against a file already at
// the cap rotates on its first write instead of doubling the file.
func (f *File) open() error {
	fh, err := os.OpenFile(f.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	fi, err := fh.Stat()
	if err != nil {
		fh.Close()
		return err
	}
	f.file, f.size = fh, fi.Size()
	return nil
}

// rotate ages the archives and starts a fresh live file. Called with f.mu
// held.
//
// The handle is closed BEFORE the rename, which is the one place this
// differs from the descriptor-based rotate() above. Go's os.OpenFile does
// not request FILE_SHARE_DELETE, so on Windows renaming a file the process
// still holds open fails with a sharing violation. Closing first is safe
// here in a way it would not be there: this process is the file's only
// writer and f.mu is held, so no record can be written into the window —
// the mutex provides what the dup2 ordering provides for a descriptor
// somebody else opened.
func (f *File) rotate(p Policy) error {
	closeErr := f.file.Close()
	f.file = nil

	stageErr := f.stage(p)

	// Reopen whatever the staging did, so the process has somewhere to
	// write on the way out of here. A failure leaves f.file nil and the
	// next Write retries.
	if err := f.open(); err != nil {
		return errors.Join(closeErr, stageErr, err)
	}
	if err := errors.Join(closeErr, stageErr); err != nil {
		return err
	}
	if err := compress(f.path+stagedSuffix, archiveName(f.path, 0)); err != nil {
		return err
	}
	// The live file turned over, so whatever was wrong before is not wrong
	// now: let the next failure speak again.
	f.noted = false
	return nil
}

// stage ages the archives and moves the live file into the staging slot,
// leaving <path> free for open to recreate.
func (f *File) stage(p Policy) error {
	// A leftover from a crash we could not compress at open time would be
	// destroyed by the rename below, so try again and give up the rotation
	// rather than overwrite real log data.
	if err := recoverStaged(f.path, p); err != nil {
		return err
	}
	if err := shiftArchives(f.path, p.Keep); err != nil {
		return err
	}
	return os.Rename(f.path, f.path+stagedSuffix)
}

// noteRotationFailure writes the failure into the log file itself and
// returns how many bytes it added. Called with f.mu held and with a usable
// handle.
//
// Into the file rather than through slog: this writer IS where slog's
// records go, so logging here would re-enter Write and deadlock on f.mu.
// The note also lands where whoever reads an oversized log will look for
// the explanation. Shaped as one JSON object so it parses like every other
// record in the file.
func (f *File) noteRotationFailure(err error) int64 {
	if f.file == nil {
		return 0
	}
	// The write error is dropped: this is the fallback for a failure we
	// already could not report, and the record the caller actually wanted
	// logged still goes out right after.
	n, _ := fmt.Fprintf(f.file, `{"time":%q,"level":"WARN","msg":"log rotation failed; this file may grow past its cap","path":%q,"err":%q}`+"\n",
		time.Now().Format(time.RFC3339), f.path, err.Error())
	return int64(n)
}

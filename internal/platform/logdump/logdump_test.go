package logdump

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServiceLogCommand_PerOS(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	since := time.Hour

	t.Run("linux", func(t *testing.T) {
		name, args := serviceLogCommand("linux", since, now)
		if name != "journalctl" {
			t.Fatalf("name = %q, want journalctl", name)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "-u waired-agent") {
			t.Errorf("args missing unit: %v", args)
		}
		if !strings.Contains(joined, "2026-07-23 11:00:00") {
			t.Errorf("args missing --since one hour before now: %v", args)
		}
	})

	t.Run("darwin", func(t *testing.T) {
		name, args := serviceLogCommand("darwin", since, now)
		if name != "log" {
			t.Fatalf("name = %q, want log", name)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, `process == "waired-agent"`) {
			t.Errorf("args missing predicate: %v", args)
		}
		if !strings.Contains(joined, "60m") {
			t.Errorf("args missing --last 60m: %v", args)
		}
	})

	t.Run("windows", func(t *testing.T) {
		name, args := serviceLogCommand("windows", since, now)
		if name != "powershell" {
			t.Fatalf("name = %q, want powershell", name)
		}
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "ProviderName='waired-agent'") {
			t.Errorf("args missing provider: %v", args)
		}
		if !strings.Contains(joined, "AddSeconds(-3600)") {
			t.Errorf("args missing 3600s window: %v", args)
		}
	})

	t.Run("unknown", func(t *testing.T) {
		name, _ := serviceLogCommand("plan9", since, now)
		if name != "" {
			t.Errorf("name = %q, want empty for unknown OS", name)
		}
	})
}

func TestServiceLogCommand_MinimumWindow(t *testing.T) {
	now := time.Unix(0, 0)
	// A sub-minute window must still ask for at least 1m on darwin and 1s
	// on windows (0 would be rejected / meaningless).
	_, dargs := serviceLogCommand("darwin", time.Second, now)
	if !strings.Contains(strings.Join(dargs, " "), "1m") {
		t.Errorf("darwin sub-minute window should clamp to 1m: %v", dargs)
	}
	_, wargs := serviceLogCommand("windows", time.Millisecond, now)
	if !strings.Contains(strings.Join(wargs, " "), "AddSeconds(-1)") {
		t.Errorf("windows sub-second window should clamp to 1s: %v", wargs)
	}
}

func TestCollectEngineLogs(t *testing.T) {
	dir := t.TempDir()
	ollamaLogs := filepath.Join(dir, "runtimes", "ollama", "logs")
	if err := os.MkdirAll(ollamaLogs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ollamaLogs, "engine.log"), []byte("ollama line one"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-.log file must be ignored.
	if err := os.WriteFile(filepath.Join(ollamaLogs, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	collectEngineLogs(&buf, dir)
	out := buf.String()
	if !strings.Contains(out, "engine.log") || !strings.Contains(out, "ollama line one") {
		t.Errorf("engine.log not collected; got:\n%s", out)
	}
	if strings.Contains(out, "ignore me") {
		t.Errorf("non-.log file was collected; got:\n%s", out)
	}
}

func TestCollectEngineLogs_NoStateDir(t *testing.T) {
	var buf bytes.Buffer
	collectEngineLogs(&buf, "")
	if !strings.Contains(buf.String(), "skipping engine logs") {
		t.Errorf("want skip note, got: %s", buf.String())
	}
}

func TestCollectEngineLogs_None(t *testing.T) {
	var buf bytes.Buffer
	collectEngineLogs(&buf, t.TempDir())
	if !strings.Contains(buf.String(), "no engine logs found") {
		t.Errorf("want 'no engine logs found', got: %s", buf.String())
	}
}

func TestCollect_WritesHeaderAndEngineLogs(t *testing.T) {
	dir := t.TempDir()
	logs := filepath.Join(dir, "runtimes", "vllm", "logs")
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "engine.log"), []byte("vllm booted"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := Collect(context.Background(), &buf, Options{StateDir: dir, Since: 30 * time.Minute}); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"waired log bundle", "service log", "engine logs", "vllm booted"} {
		if !strings.Contains(out, want) {
			t.Errorf("bundle missing %q; got:\n%s", want, out)
		}
	}
}

// TestServiceLogFiles_PerOS pins which platforms keep the service's
// stdout/stderr in plain files. Only launchd does; systemd and the SCM
// route them to the journal and the Event Log, which serviceLogCommand
// already covers. Product contract (#331).
func TestServiceLogFiles_PerOS(t *testing.T) {
	for _, tc := range []struct {
		name, goos, home string
		want             []string
	}{
		{"darwin", "darwin", "/Users/example", []string{
			"/Library/Logs/waired-agent.err.log",
			"/Library/Logs/waired-agent.out.log",
			"/Users/example/Library/Logs/waired-tray.err.log",
			"/Users/example/Library/Logs/waired-tray.out.log",
		}},
		{"darwin without a home", "darwin", "", []string{
			"/Library/Logs/waired-agent.err.log",
			"/Library/Logs/waired-agent.out.log",
		}},
		{"linux", "linux", "/home/example", nil},
		{"windows", "windows", `C:\Users\example`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := serviceLogFiles(tc.goos, tc.home)
			if len(got) != len(tc.want) {
				t.Fatalf("serviceLogFiles(%q, %q) = %v, want %v", tc.goos, tc.home, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("file %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// writeGz is the archive-shaped fixture the rotation produces.
func writeGz(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	if _, err := io.WriteString(zw, content); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
}

// TestAppendServiceLogFiles_OldestArchiveFirst covers the whole
// collection: archives are decompressed, ordered oldest to newest, and
// the live file comes last, so the bundle reads forward in time.
// Product contract.
func TestAppendServiceLogFiles_OldestArchiveFirst(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "waired-agent.err.log")
	if err := os.WriteFile(base, []byte("live line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeGz(t, base+".0.gz", "newer archive\n")
	writeGz(t, base+".1.gz", "older archive\n")

	var buf bytes.Buffer
	appendServiceLogFiles(&buf, []string{base})
	out := buf.String()

	for _, want := range []string{"older archive", "newer archive", "live line"} {
		if !strings.Contains(out, want) {
			t.Fatalf("bundle missing %q:\n%s", want, out)
		}
	}
	older := strings.Index(out, "older archive")
	newer := strings.Index(out, "newer archive")
	live := strings.Index(out, "live line")
	if older >= newer || newer >= live {
		t.Errorf("order is older=%d newer=%d live=%d, want ascending", older, newer, live)
	}
	if !strings.Contains(out, base+".1.gz") || !strings.Contains(out, base) {
		t.Errorf("bundle is missing per-file headers:\n%s", out)
	}
}

// TestAppendServiceLogFiles_MissingFilesAreSkipped: no archives yet, no
// tray installed, an agent that never started — all normal, and none of
// them should put an error in a user's bug report.
func TestAppendServiceLogFiles_MissingFilesAreSkipped(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	appendServiceLogFiles(&buf, []string{filepath.Join(dir, "absent.log")})
	if out := buf.String(); out != "" {
		t.Errorf("absent file produced output: %q", out)
	}
}

// TestWarnLegacyRotationGap fires on the pre-#331 host state — a live
// file holding only newsyslog's turn-over banner, with archives beside
// it — and stays quiet when the live file has real content. Without the
// note, a bundle from such a host reads as "the daemon logged nothing".
// Product contract.
func TestWarnLegacyRotationGap(t *testing.T) {
	const banner = "Aug  1 03:30:01 host newsyslog[39124]: logfile turned over due to size>1024K\n"

	t.Run("banner only, archives present", func(t *testing.T) {
		dir := t.TempDir()
		base := filepath.Join(dir, "waired-agent.err.log")
		if err := os.WriteFile(base, []byte(banner), 0o644); err != nil {
			t.Fatal(err)
		}
		writeGz(t, base+".0.gz", "the lines that survived\n")

		var buf bytes.Buffer
		appendServiceLogFiles(&buf, []string{base})
		if !strings.Contains(buf.String(), "#331") {
			t.Errorf("no gap warning for a banner-only live file:\n%s", buf.String())
		}
	})

	t.Run("live file has real content", func(t *testing.T) {
		dir := t.TempDir()
		base := filepath.Join(dir, "waired-agent.err.log")
		if err := os.WriteFile(base, []byte(banner+`{"msg":"still logging"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeGz(t, base+".0.gz", "older\n")

		var buf bytes.Buffer
		appendServiceLogFiles(&buf, []string{base})
		if strings.Contains(buf.String(), "#331") {
			t.Errorf("gap warned about a file that is still being written:\n%s", buf.String())
		}
	})

	t.Run("no archives", func(t *testing.T) {
		dir := t.TempDir()
		base := filepath.Join(dir, "waired-agent.err.log")
		if err := os.WriteFile(base, []byte(banner), 0o644); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		appendServiceLogFiles(&buf, []string{base})
		if strings.Contains(buf.String(), "#331") {
			t.Error("warned about a rotation gap with no archives to point at")
		}
	})
}

// TestReadTail_KeepsTheEndOnALineBoundary: the cap exists so an
// unrotated legacy file cannot dominate (or be read wholly into) a
// bundle, and the kept region must not open mid-record.
func TestReadTail_KeepsTheEndOnALineBoundary(t *testing.T) {
	var src strings.Builder
	for i := range 1000 {
		fmt.Fprintf(&src, "line %04d padded out to make this worth truncating\n", i)
	}
	data, truncated, err := readTail(strings.NewReader(src.String()), 200)
	if err != nil {
		t.Fatalf("readTail: %v", err)
	}
	if !truncated {
		t.Fatal("readTail did not report truncation")
	}
	if len(data) > 200 {
		t.Errorf("kept %d bytes, want <= 200", len(data))
	}
	if !strings.HasPrefix(string(data), "line ") {
		t.Errorf("kept region opens mid-line: %q", string(data)[:20])
	}
	if !strings.HasSuffix(string(data), "line 0999 padded out to make this worth truncating\n") {
		t.Errorf("kept region is not the tail: %q", string(data))
	}
}

func TestReadTail_ShortInputIsUntouched(t *testing.T) {
	data, truncated, err := readTail(strings.NewReader("one\ntwo\n"), 1024)
	if err != nil || truncated {
		t.Fatalf("readTail = (%q, %v, %v), want untruncated", data, truncated, err)
	}
	if string(data) != "one\ntwo\n" {
		t.Errorf("data = %q", data)
	}
}

// TestArchivesFor_SortsOldestFirst records the archive naming the
// rotation writes: a higher index is older, so reading order is
// descending by index.
func TestArchivesFor_SortsOldestFirst(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "waired-agent.err.log")
	for _, n := range []string{"0", "1", "2"} {
		writeGz(t, base+"."+n+".gz", "archive "+n)
	}
	got := archivesFor(base)
	want := []string{base + ".2.gz", base + ".1.gz", base + ".0.gz"}
	if len(got) != len(want) {
		t.Fatalf("archivesFor = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("archive %d = %q, want %q", i, got[i], want[i])
		}
	}
}

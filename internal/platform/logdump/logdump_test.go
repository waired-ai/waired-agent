package logdump

import (
	"bytes"
	"context"
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

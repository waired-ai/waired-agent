package detect

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/waired-ai/waired-agent/internal/integration/shellalias"
)

func writeRC(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestShellAlias_NoFiles(t *testing.T) {
	got := ShellAlias(t.TempDir(), "/p/waired claude")
	if len(got) != 0 {
		t.Errorf("expected empty result with no rc files, got %+v", got)
	}
}

func TestShellAlias_FreshInstall(t *testing.T) {
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	body := "# user\n" + shellalias.SentinelOpen + "\n" +
		`alias claude='/p/waired claude'` + "\n" + shellalias.SentinelClose + "\n"
	writeRC(t, bashrc, body)

	got := ShellAlias(home, "/p/waired claude")
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1: %+v", len(got), got)
	}
	r := got[0]
	if !r.Configured || r.Stale {
		t.Errorf("expected configured & not stale: %+v", r)
	}
	if r.CurrentValue != "/p/waired claude" {
		t.Errorf("CurrentValue = %q", r.CurrentValue)
	}
}

func TestShellAlias_StalePath(t *testing.T) {
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	body := shellalias.SentinelOpen + "\n" +
		`alias claude='/old/waired claude'` + "\n" + shellalias.SentinelClose + "\n"
	writeRC(t, bashrc, body)

	got := ShellAlias(home, "/new/waired claude")
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if !got[0].Stale {
		t.Errorf("expected stale: %+v", got[0])
	}
}

func TestShellAlias_Unparseable(t *testing.T) {
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	body := shellalias.SentinelOpen + "\n# nothing\n" + shellalias.SentinelClose + "\n"
	writeRC(t, bashrc, body)

	got := ShellAlias(home, "/p/waired claude")
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if !got[0].Configured || !got[0].Stale || got[0].Note != "unparseable" {
		t.Errorf("expected configured+stale+unparseable note: %+v", got[0])
	}
}

func TestShellAlias_RCWithoutBlock(t *testing.T) {
	home := t.TempDir()
	bashrc := filepath.Join(home, ".bashrc")
	writeRC(t, bashrc, "# user\nexport PATH=$PATH:/opt/x\n")

	got := ShellAlias(home, "/p/waired claude")
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if got[0].Configured {
		t.Errorf("expected not configured: %+v", got[0])
	}
}

func TestShellAlias_FishOK(t *testing.T) {
	home := t.TempDir()
	fish := filepath.Join(home, ".config", "fish", "config.fish")
	body := shellalias.SentinelOpen + "\n" +
		`function claude --description 'route claude through waired'; '/p/waired' claude $argv; end` + "\n" +
		shellalias.SentinelClose + "\n"
	writeRC(t, fish, body)

	got := ShellAlias(home, "/p/waired claude")
	if len(got) != 1 || got[0].Stale || !got[0].Configured {
		t.Fatalf("got %+v", got)
	}
}

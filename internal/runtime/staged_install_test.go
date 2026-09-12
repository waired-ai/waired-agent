package runtime

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// PRODUCT CONTRACT (waired-agent#1309): the engine binary never exists at
// the path the daemon watches until the whole install is there.
//
// The daemon decides an engine is installed by stat-ing that one path
// (engine_resolve.go's resolveOllamaBinary: os.Stat + IsRegular, no size
// and no exec bit) and polls it every two seconds; the false->true edge
// starts the engine. An extractor writing in place makes the path a
// regular file when tar opens it, minutes before the bytes are all there
// — so on macOS the first spawn died with "fork/exec: malformed Mach-o
// file" and the terminal printed "The inference engine won't start."
//
// The fake extractor here is the defect boundary: it publishes a
// half-written binary and only then completes it, which is what an untar
// of a ~1 GB universal binary does over several seconds.
func TestOllamaInstaller_BinaryIsNeverVisibleHalfWritten(t *testing.T) {
	lowerFloor(t)

	base := t.TempDir()
	inst := NewOllamaInstaller(base)
	stubRelease(t, inst, []byte("BIGENOUGH"))
	rel := hostRelease(t)

	// What an outside reader (the daemon's two-second poll) would have
	// seen at the moment the extractor was half way through.
	var sawPartial bool
	var partialSize int64
	inst.extractFn = func(_, destDir string) error {
		bin := filepath.Join(destDir, OllamaBinaryName(runtime.GOOS))
		if rel.ExtractSub == "" {
			bin = filepath.Join(destDir, "bin", OllamaBinaryName(runtime.GOOS))
		}
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			return err
		}
		// Half a binary, as tar leaves it mid-stream.
		if err := os.WriteFile(bin, []byte("#!/bi"), 0o755); err != nil {
			return err
		}
		if fi, err := os.Stat(inst.BinaryPath()); err == nil && fi.Mode().IsRegular() {
			sawPartial, partialSize = true, fi.Size()
		}
		// The rest of the bytes.
		return os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	}

	if err := inst.Install(context.Background(), nil); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if sawPartial {
		t.Errorf("the daemon's path held a %d-byte regular file while the extract was in flight", partialSize)
	}
	if !inst.Active() {
		t.Fatalf("Active() false after install; expected %s executable", inst.BinaryPath())
	}
	b, err := os.ReadFile(inst.BinaryPath())
	if err != nil {
		t.Fatalf("read the promoted binary: %v", err)
	}
	if string(b) != "#!/bin/sh\nexit 0\n" {
		t.Errorf("promoted binary = %q, want the complete one", b)
	}
}

// PRODUCT CONTRACT: promotion replaces the engine payload and nothing
// else.
//
// The two rules differ by what the destination directory holds, and the
// consequence of applying the wrong one on Linux is deleting the model
// store: there destDir is the install's BASE directory, shared with
// models/, the engine's logs and the HOME it is spawned with. On macOS
// and Windows it is BaseDir/bin, which holds the payload alone — so a
// library the previous version shipped and this one dropped has to go,
// which is what #190 established for Windows.
func TestPromoteStagedInstall_ClearsThePayloadAndSparesTheNeighbours(t *testing.T) {
	stage := func(t *testing.T) (staged, dest string) {
		t.Helper()
		root := t.TempDir()
		staged, dest = filepath.Join(root, "staged"), filepath.Join(root, "dest")
		mustWrite(t, filepath.Join(staged, "bin", "ollama"), "new")
		mustWrite(t, filepath.Join(staged, "lib", "new.so"), "new")
		// The previous install: one file this version still ships, one it
		// dropped.
		mustWrite(t, filepath.Join(dest, "bin", "ollama"), "old")
		mustWrite(t, filepath.Join(dest, "lib", "dropped.so"), "old")
		// A neighbour that is not the payload — on Linux this is the model
		// store and the engine's logs.
		mustWrite(t, filepath.Join(dest, "models", "blobs", "sha256-abc"), "20 GB of weights")
		mustWrite(t, filepath.Join(dest, "logs", "engine.log"), "yesterday")
		return staged, dest
	}

	t.Run("a shared directory keeps its neighbours", func(t *testing.T) {
		staged, dest := stage(t)
		if err := promoteStagedInstall(staged, dest, false); err != nil {
			t.Fatalf("promote: %v", err)
		}
		mustRead(t, filepath.Join(dest, "bin", "ollama"), "new")
		mustRead(t, filepath.Join(dest, "models", "blobs", "sha256-abc"), "20 GB of weights")
		mustRead(t, filepath.Join(dest, "logs", "engine.log"), "yesterday")
		// Still replaced wholesale INSIDE the payload's own directories: a
		// stale library the new binary could load is the #190 hazard.
		if _, err := os.Stat(filepath.Join(dest, "lib", "dropped.so")); !os.IsNotExist(err) {
			t.Errorf("a library the new version dropped survived (err=%v)", err)
		}
	})

	t.Run("an exclusive directory is emptied first", func(t *testing.T) {
		staged, dest := stage(t)
		if err := promoteStagedInstall(staged, dest, true); err != nil {
			t.Fatalf("promote: %v", err)
		}
		mustRead(t, filepath.Join(dest, "bin", "ollama"), "new")
		for _, gone := range []string{"models", "logs"} {
			if _, err := os.Stat(filepath.Join(dest, gone)); !os.IsNotExist(err) {
				t.Errorf("%s survived a wholesale clear (err=%v)", gone, err)
			}
		}
	})
}

// PRODUCT CONTRACT: the staged tree is what gets promoted, so a promotion
// into an empty destination is an ordinary first install.
func TestPromoteStagedInstall_FirstInstall(t *testing.T) {
	root := t.TempDir()
	staged, dest := filepath.Join(root, "staged"), filepath.Join(root, "dest")
	mustWrite(t, filepath.Join(staged, "ollama"), "new")

	if err := promoteStagedInstall(staged, dest, true); err != nil {
		t.Fatalf("promote: %v", err)
	}
	mustRead(t, filepath.Join(dest, "ollama"), "new")
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(b) != want {
		t.Errorf("%s = %q, want %q", path, b, want)
	}
}

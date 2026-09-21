//go:build linux || darwin

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Two holders of the install lock exclude each other across open files, as
// two processes are excluded, and a waiter is told it is waiting
// (waired-agent#1511).
func TestOllamaInstallerLock_ExcludesASecondHolder(t *testing.T) {
	dir := t.TempDir()
	a, b := NewOllamaInstaller(dir), NewOllamaInstaller(dir)
	unlock, err := a.Lock(context.Background(), nil)
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	waited := 0
	ctx, cancel := context.WithTimeout(context.Background(), 3*ollamaLockPoll)
	defer cancel()
	if _, err := b.Lock(ctx, func() { waited++ }); err == nil {
		t.Fatal("a second holder got the lock while the first held it")
	}
	if waited != 1 {
		t.Errorf("onWait called %d times, want once", waited)
	}
	unlock()
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	unlock2, err := b.Lock(ctx2, nil)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	unlock2()
	// The sidecar sits beside the engine, outside what an install replaces.
	if _, err := os.Stat(filepath.Join(dir, ollamaInstallLockName)); err != nil {
		t.Errorf("lock sidecar: %v", err)
	}
}

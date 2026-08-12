package atomicfile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

// PRODUCT CONTRACT (waired-agent#698): a reader holding the destination open
// must not lose the write. This is the whole reason the package exists, and
// it is the case that cannot be expressed on Unix — there, the rename simply
// succeeds and the test proves nothing.
//
// A read-only open is deliberate. Go's os.Open does not request
// FILE_SHARE_DELETE, so one reader is enough to make Windows refuse the
// replacing rename; "only a writer is dangerous" is the intuition this
// measured on real NTFS and disproved
// (docs/knowledges/20260812/0120-windows-rename-open-handle.md).
func TestReplace_SurvivesAReaderHoldingTheDestination(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "state")
	if err := os.WriteFile(dst, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, ".state-1")
	if err := os.WriteFile(src, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}

	held, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	// Release inside the retry budget. Replace has ~200 ms of attempts; the
	// close happens on this goroutine's next scheduling slot, orders of
	// magnitude inside it.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = held.Close()
	}()

	if err := Replace(src, dst); err != nil {
		t.Fatalf("Replace lost the race with a reader: %v", err)
	}
	<-done

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "after" {
		t.Errorf("destination = %q, want %q", got, "after")
	}
}

// The plain os.Rename this package wraps really does fail in that situation.
// Without this the test above could pass on a Windows that had stopped
// refusing the rename, and would then be proving nothing — the #178 shape.
func TestOsRename_FailsWhileAReaderHoldsTheDestination(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "state")
	if err := os.WriteFile(dst, []byte("before"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, ".state-1")
	if err := os.WriteFile(src, []byte("after"), 0o644); err != nil {
		t.Fatal(err)
	}

	held, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	err = os.Rename(src, dst)
	if err == nil {
		t.Fatal("os.Rename replaced a destination held open for read — the premise of this package no longer holds")
	}
	if !replaceBlocked(err) {
		t.Errorf("replaceBlocked(%v) = false, want true — the retry would not fire for the error Windows actually returns", err)
	}
}

func TestReplaceBlocked(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"destination held", syscall.ERROR_ACCESS_DENIED, true},
		{"source held", syscall.Errno(windows.ERROR_SHARING_VIOLATION), true},
		{"wrapped by os.LinkError", &os.LinkError{Op: "rename", Err: syscall.ERROR_ACCESS_DENIED}, true},
		{"missing file", syscall.ERROR_FILE_NOT_FOUND, false},
		{"not an errno at all", errors.New("some other failure"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := replaceBlocked(tc.err); got != tc.want {
				t.Errorf("replaceBlocked(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

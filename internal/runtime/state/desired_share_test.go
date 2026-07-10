package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Missing desired-share file returns the empty state so callers can
// fall back to agentconfig.Inference.ShareWithMesh — that's the
// "never touched the toggle" signal.
func TestReadDesiredShareMeshMissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	got, err := ReadDesiredShareMesh(dir)
	if err != nil {
		t.Fatalf("ReadDesiredShareMesh: %v", err)
	}
	if got != "" {
		t.Errorf("missing file should return empty state, got %q", got)
	}
}

func TestReadDesiredShareMeshRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesiredShareMesh(dir, ShareMeshNotShared); err != nil {
		t.Fatalf("WriteDesiredShareMesh not_shared: %v", err)
	}
	got, err := ReadDesiredShareMesh(dir)
	if err != nil {
		t.Fatalf("ReadDesiredShareMesh: %v", err)
	}
	if got != ShareMeshNotShared {
		t.Errorf("got %q, want %q", got, ShareMeshNotShared)
	}

	if err := WriteDesiredShareMesh(dir, ShareMeshShared); err != nil {
		t.Fatalf("WriteDesiredShareMesh shared: %v", err)
	}
	got, err = ReadDesiredShareMesh(dir)
	if err != nil {
		t.Fatalf("ReadDesiredShareMesh: %v", err)
	}
	if got != ShareMeshShared {
		t.Errorf("got %q, want %q", got, ShareMeshShared)
	}
}

func TestReadDesiredShareMeshTolerantOfWhitespace(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredSharePath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredSharePath(dir), []byte("  not_shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDesiredShareMesh(dir)
	if err != nil {
		t.Fatalf("ReadDesiredShareMesh: %v", err)
	}
	if got != ShareMeshNotShared {
		t.Errorf("got %q, want %q", got, ShareMeshNotShared)
	}
}

func TestReadDesiredShareMeshEmptyMeansEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredSharePath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredSharePath(dir), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadDesiredShareMesh(dir)
	if err != nil {
		t.Fatalf("ReadDesiredShareMesh: %v", err)
	}
	if got != "" {
		t.Errorf("got %q, want empty (an empty file is indistinguishable from no choice)", got)
	}
}

func TestReadDesiredShareMeshUnknownErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(DesiredSharePath(dir)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DesiredSharePath(dir), []byte("paused\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadDesiredShareMesh(dir); err == nil {
		t.Error("expected error for unknown share state value, got nil")
	}
}

func TestWriteDesiredShareMeshRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesiredShareMesh(dir, ShareMeshState("paused")); err == nil {
		t.Error("expected error for invalid share state, got nil")
	}
	if _, err := os.Stat(DesiredSharePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file should not be created for invalid value, stat err=%v", err)
	}
}

// Empty state must also be rejected at write time — callers should
// pass an explicit ShareMeshShared / ShareMeshNotShared and never
// "clear" the file by writing "".
func TestWriteDesiredShareMeshRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDesiredShareMesh(dir, ShareMeshState("")); err == nil {
		t.Error("expected error for empty share state, got nil")
	}
}

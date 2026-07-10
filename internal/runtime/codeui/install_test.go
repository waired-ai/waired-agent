package codeui

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func makeTarGz(t *testing.T, entry string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: entry, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeZip(t *testing.T, entry string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(entry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestExtractBinaryFromTarGz(t *testing.T) {
	content := []byte("#!/bin/sh\necho opencode\n")
	body := makeTarGz(t, "opencode", content)
	dst := filepath.Join(t.TempDir(), "opencode")
	if err := extractBinaryFromTarGz(body, dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: %q", got)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(dst); fi.Mode().Perm()&0o111 == 0 {
			t.Error("binary not executable")
		}
	}
}

func TestExtractBinaryFromZip(t *testing.T) {
	content := []byte("MZ-fake-windows-binary")
	body := makeZip(t, "opencode.exe", content)
	dst := filepath.Join(t.TempDir(), "opencode.exe")
	if err := extractBinaryFromZip(body, dst); err != nil {
		t.Fatalf("extract: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("content mismatch: %q", got)
	}
}

func TestExtract_NoBinaryEntry(t *testing.T) {
	body := makeTarGz(t, "README.md", []byte("hi"))
	if err := extractBinaryFromTarGz(body, filepath.Join(t.TempDir(), "opencode")); err == nil {
		t.Fatal("want error when archive has no opencode entry")
	}
}

func TestInstall_LocalBinary_SkipsDownload(t *testing.T) {
	dir := t.TempDir()
	inst := NewInstaller(dir)
	inst.LocalBinary = "/usr/local/bin/opencode"
	inst.downloadFn = func(context.Context, string) ([]byte, error) {
		t.Fatal("LocalBinary must skip download")
		return nil, nil
	}
	if err := inst.Install(context.Background(), nil); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if inst.BinaryPath() != "/usr/local/bin/opencode" {
		t.Errorf("BinaryPath = %q, want the LocalBinary override", inst.BinaryPath())
	}
	for _, d := range []string{inst.OpenCodeConfigDir(), inst.DataDir(), inst.LogDir()} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("dir %s not created: %v", d, err)
		}
	}
}

func TestInstall_SHA256Mismatch(t *testing.T) {
	if _, ok := artifacts[runtime.GOOS+"/"+runtime.GOARCH]; !ok {
		t.Skipf("no artifact for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	defer func(v int) { archiveMinBytes = v }(archiveMinBytes)
	archiveMinBytes = 1
	inst := NewInstaller(t.TempDir())
	inst.downloadFn = func(context.Context, string) ([]byte, error) {
		return bytes.Repeat([]byte("x"), 64), nil // wrong sha
	}
	err := inst.Install(context.Background(), nil)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("sha256 mismatch")) {
		t.Fatalf("want sha256 mismatch error, got %v", err)
	}
}

func TestInstall_HappyPath(t *testing.T) {
	key := runtime.GOOS + "/" + runtime.GOARCH
	art, ok := artifacts[key]
	if !ok {
		t.Skipf("no artifact for %s", key)
	}
	content := []byte("fake-opencode-binary-body")
	var body []byte
	if art.isZip {
		body = makeZip(t, "opencode", content)
	} else {
		body = makeTarGz(t, "opencode", content)
	}
	// Point the platform artifact's pinned sha at our synthetic archive.
	defer func(saved platformArtifact) { artifacts[key] = saved }(art)
	patched := art
	patched.sha256 = sha256hex(body)
	artifacts[key] = patched
	defer func(v int) { archiveMinBytes = v }(archiveMinBytes)
	archiveMinBytes = 1

	inst := NewInstaller(t.TempDir())
	inst.downloadFn = func(context.Context, string) ([]byte, error) { return body, nil }

	var stages []string
	if err := inst.Install(context.Background(), func(p InstallProgress) { stages = append(stages, p.Stage) }); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !inst.Active() {
		t.Fatal("Active() false after install")
	}
	got, err := os.ReadFile(inst.BinaryPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("binary content mismatch")
	}
	// The verify stage must run between download and extract.
	joined := ""
	for _, s := range stages {
		joined += s + " "
	}
	for _, want := range []string{"download", "verify", "extract", "activate"} {
		if !bytes.Contains([]byte(joined), []byte(want)) {
			t.Errorf("missing progress stage %q (stages=%v)", want, stages)
		}
	}
}

func TestWriteDefaultConfig(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteDefaultConfig(dir)
	if err != nil {
		t.Fatalf("WriteDefaultConfig: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"model": "waired/default"`)) {
		t.Errorf("opencode.json missing default model: %s", b)
	}
	// Idempotent.
	if _, err := WriteDefaultConfig(dir); err != nil {
		t.Fatalf("second WriteDefaultConfig: %v", err)
	}
}

// TestInstall_VersionDrift_Reinstalls proves a waired upgrade (bumped pin)
// replaces a stale binary: after install the version marker matches the pin
// and NeedsInstall is false; once the marker is rewound to an old version,
// NeedsInstall flips true and Install downloads again.
func TestInstall_VersionDrift_Reinstalls(t *testing.T) {
	key := runtime.GOOS + "/" + runtime.GOARCH
	art, ok := artifacts[key]
	if !ok {
		t.Skipf("no artifact for %s", key)
	}
	content := []byte("fake-opencode-binary-body-v2")
	var body []byte
	if art.isZip {
		body = makeZip(t, "opencode", content)
	} else {
		body = makeTarGz(t, "opencode", content)
	}
	defer func(saved platformArtifact) { artifacts[key] = saved }(art)
	patched := art
	patched.sha256 = sha256hex(body)
	artifacts[key] = patched
	defer func(v int) { archiveMinBytes = v }(archiveMinBytes)
	archiveMinBytes = 1

	inst := NewInstaller(t.TempDir())
	var downloads int
	inst.downloadFn = func(context.Context, string) ([]byte, error) { downloads++; return body, nil }

	if err := inst.Install(context.Background(), nil); err != nil {
		t.Fatalf("install: %v", err)
	}
	if downloads != 1 {
		t.Fatalf("downloads = %d, want 1", downloads)
	}
	if inst.InstalledVersion() != OpenCodePinnedVersion {
		t.Errorf("InstalledVersion = %q, want %q", inst.InstalledVersion(), OpenCodePinnedVersion)
	}
	if inst.NeedsInstall() {
		t.Error("NeedsInstall true right after a current install")
	}

	// A second install with the matching pin is a no-op (no re-download).
	if err := inst.Install(context.Background(), nil); err != nil {
		t.Fatalf("second install: %v", err)
	}
	if downloads != 1 {
		t.Errorf("downloads = %d after up-to-date install, want 1", downloads)
	}

	// Simulate a waired upgrade: the on-disk binary now predates the pin.
	if err := os.WriteFile(inst.versionFilePath(), []byte("0.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !inst.NeedsInstall() {
		t.Fatal("NeedsInstall false despite version drift")
	}
	if err := inst.Install(context.Background(), nil); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	if downloads != 2 {
		t.Errorf("downloads = %d after drift, want 2 (re-downloaded)", downloads)
	}
	if inst.InstalledVersion() != OpenCodePinnedVersion {
		t.Errorf("InstalledVersion = %q after reinstall, want pin", inst.InstalledVersion())
	}
}

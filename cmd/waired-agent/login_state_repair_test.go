package main

import (
	"bytes"
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/identity"
	"github.com/waired-ai/waired-agent/internal/management"
)

// repairController builds a login controller whose state dir is a temp
// dir and whose live identity is whatever the caller supplies. Deliberately
// not a whole session: the repair reads one field of one struct, and
// standing up an engine and a state writer to reach it would put a fake at
// the defect boundary rather than below it.
func repairController(t *testing.T, stateDir string, id *identity.Identity) *loginController {
	t.Helper()
	sb := &switchboard{}
	lc := newLoginController(sb, loginControllerConfig{
		StateDir:          stateDir,
		DefaultControlURL: "https://cp.example",
		Endpoint:          "udp4:127.0.0.1:0",
		RootCtx:           context.Background(),
		Logger:            testLogger(),
	})
	lc.liveIdentity = func() *identity.Identity { return id }
	return lc
}

func repairIdentity() *identity.Identity {
	return &identity.Identity{
		DeviceID:     "dev_test",
		DeviceName:   "test-host",
		AccountEmail: "someone@example.com",
		NetworkID:    "wn_test",
		ControlURL:   "https://app.dev.example",
		OverlayIP:    "100.64.0.7",
	}
}

// PRODUCT CONTRACT — waired-agent#800. A daemon whose state dir was
// removed under it must put identity.json back when someone runs `waired
// init`, instead of answering "already signed in — resuming setup" over a
// state dir that no longer holds the identity that answer is drawn from.
func TestRestoreIdentity_RewritesAnAbsentFile(t *testing.T) {
	dir := t.TempDir()
	id := repairIdentity()
	lc := repairController(t, dir, id)

	lc.restoreIdentityIfMissing()

	got, err := identity.Load(dir)
	if err != nil {
		t.Fatalf("Load after repair: %v", err)
	}
	if got == nil {
		t.Fatal("identity.json was not restored")
	}
	if got.DeviceID != id.DeviceID || got.ControlURL != id.ControlURL {
		t.Errorf("restored identity = %+v, want device %q control %q",
			got, id.DeviceID, id.ControlURL)
	}
}

// The repair also recreates the state dir tree, which is what gives the
// engine its $HOME back. On macOS and Linux the state dir IS the ollama
// HOME, so a wiped dir takes the registry key with it and every model
// pull fails until the daemon restarts — the second half of #800's item 2.
//
// Asserted on the directory rather than on ollama's key: the key is
// ollama's to write, and it writes it on its next pull. What we owe it is
// a directory to write into.
func TestRestoreIdentity_RecreatesTheStateDirTree(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "waired")
	// Never created — the #800 shape is the whole tree being gone.
	lc := repairController(t, dir, repairIdentity())

	lc.restoreIdentityIfMissing()

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("state dir was not recreated: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
}

// A file that is PRESENT is never overwritten. It may be an identity
// someone put there deliberately, and clobbering it would destroy the
// only copy.
func TestRestoreIdentity_NeverOverwritesAPresentFile(t *testing.T) {
	dir := t.TempDir()
	onDisk := repairIdentity()
	onDisk.DeviceID = "dev_on_disk"
	if err := identity.Save(dir, onDisk); err != nil {
		t.Fatal(err)
	}

	inMemory := repairIdentity()
	inMemory.DeviceID = "dev_in_memory"
	repairController(t, dir, inMemory).restoreIdentityIfMissing()

	got, err := identity.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeviceID != "dev_on_disk" {
		t.Errorf("on-disk identity was overwritten: device_id = %q, want dev_on_disk", got.DeviceID)
	}
}

// An unenrolled daemon has no identity to write and must not invent one.
func TestRestoreIdentity_UnenrolledDaemonWritesNothing(t *testing.T) {
	dir := t.TempDir()
	repairController(t, dir, nil).restoreIdentityIfMissing()

	if got, err := identity.Load(dir); err != nil || got != nil {
		t.Fatalf("Load = (%v, %v), want (nil, nil) — nothing should have been written", got, err)
	}
}

// The resume answer is where the repair has to happen: that is the branch
// #800 reported, and it is reached only from a live session.
func TestLoginStart_ResumeRestoresTheIdentity(t *testing.T) {
	dir := t.TempDir()
	id := repairIdentity()

	sb := &switchboard{}
	// A published session is what makes Start take the resume branch. Only
	// the pointer's non-nil-ness is read there.
	sb.cur.Store(&session{})
	lc := newLoginController(sb, loginControllerConfig{
		StateDir:          dir,
		DefaultControlURL: "https://cp.example",
		Endpoint:          "udp4:127.0.0.1:0",
		RootCtx:           context.Background(),
		Logger:            testLogger(),
	})
	lc.liveIdentity = func() *identity.Identity { return id }

	st, err := lc.Start(context.Background(), management.LoginStartRequest{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if st.Phase != management.LoginPhaseActive {
		t.Fatalf("phase = %q, want active (the resume answer)", st.Phase)
	}
	if st.SessionID != "" {
		t.Fatalf("resume answered with session %q; it must carry none", st.SessionID)
	}
	got, err := identity.Load(dir)
	if err != nil || got == nil {
		t.Fatalf("resume did not restore identity.json: (%v, %v)", got, err)
	}
}

// The repair says so. Self-repair that is silent is the same defect one
// layer over: #800's third symptom is a fallback that reconciled quietly.
//
// PRODUCT CONTRACT — waired-agent#800, and the reason it is asserted here
// rather than on a host: on a machine whose state dir was just wiped the
// daemon's own log file went with it, so the line lands on a deleted inode
// until the daemon restarts. Verified on sv-macmini 2026-08-15 — the
// repair worked, `waired logs` had nowhere to read it from.
func TestRestoreIdentity_SaysSo(t *testing.T) {
	dir := t.TempDir()
	var buf bytes.Buffer
	lc := repairController(t, dir, repairIdentity())
	lc.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	lc.restoreIdentityIfMissing()

	out := buf.String()
	if !strings.Contains(out, "restored identity.json") {
		t.Errorf("the repair was silent: %q", out)
	}
	if !strings.Contains(out, "dev_test") {
		t.Errorf("the line does not name the device it restored: %q", out)
	}
}

// The refusal says so too, and never writes.
//
// The stat is injected rather than produced by chmod: os.Chmod on Windows
// toggles the read-only attribute and does not deny traversal, so a
// chmod-based version of this passed on linux/darwin and exercised the
// ENOENT branch on windows — reported green for the wrong reason until CI
// caught it. Injecting keeps the routing covered identically on all three.
func TestRestoreIdentity_ReportsWhatItCannotRead(t *testing.T) {
	dir := t.TempDir()
	prev := repairStat
	repairStat = func(string) (os.FileInfo, error) {
		return nil, &fs.PathError{Op: "stat", Path: "identity.json", Err: fs.ErrPermission}
	}
	t.Cleanup(func() { repairStat = prev })

	var buf bytes.Buffer
	lc := repairController(t, dir, repairIdentity())
	lc.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	lc.restoreIdentityIfMissing()

	out := buf.String()
	if !strings.Contains(out, "cannot tell whether identity.json is present") {
		t.Errorf("an unreadable state dir was not reported: %q", out)
	}
	if strings.Contains(out, "restored identity.json") {
		t.Errorf("an unreadable state dir was treated as absent and written to: %q", out)
	}
	if got, err := identity.Load(dir); err == nil && got != nil {
		t.Errorf("an unreadable state dir was written to: %+v", got)
	}
}

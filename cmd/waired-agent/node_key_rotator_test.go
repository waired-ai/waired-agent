package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/devicekeys"
	"github.com/waired-ai/waired-agent/internal/identity"
)

// fakeControlPlane models the rotate endpoint's decision the same way the
// real handler makes it: the presented old key must equal the key the CP
// currently holds, or the answer is 409 node_key_mismatch. It records
// every request so a test can assert on the arguments the subject really
// sent, not just on the outcome.
type fakeControlPlane struct {
	mu sync.Mutex
	// current is the Node Key the CP holds for the device. Tests move it
	// to model a rotation that committed, or a re-authentication that
	// wrote some other key.
	current  string
	requests []rotateRequest
}

type rotateRequest struct {
	Old string `json:"old_node_public_key"`
	New string `json:"new_node_public_key"`
	Sig string `json:"machine_signature"`
}

func (f *fakeControlPlane) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req rotateRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("control plane got undecodable body %q: %v", body, err)
		}
		f.mu.Lock()
		f.requests = append(f.requests, req)
		held := f.current
		if req.Old == held {
			f.current = req.New
		}
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if req.Old != held {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":{"type":"node_key_mismatch",` +
				`"message":"old_node_public_key does not match the device's current node key"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_certificate":  json.RawMessage(`{"version":1}`),
			"node_key_expires_at": time.Now().Add(180 * 24 * time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeControlPlane) sent() []rotateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rotateRequest(nil), f.requests...)
}

func (f *fakeControlPlane) held() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current
}

// rotatorFixture is a state dir with a current node key, a machine key,
// and a rotator wired to a fake control plane.
type rotatorFixture struct {
	t         *testing.T
	stateDir  string
	paths     *identity.Paths
	current   *devicekeys.NodeKey
	cp        *fakeControlPlane
	rotator   *nodeKeyRotator
	published string // what PublishedNodeKey() answers
	reactivs  int
	mu        sync.Mutex
}

func newRotatorFixture(t *testing.T) *rotatorFixture {
	t.Helper()
	stateDir := t.TempDir()
	paths, err := identity.PathsFor(stateDir)
	if err != nil {
		t.Fatalf("PathsFor: %v", err)
	}
	current, err := devicekeys.NewNodeKey()
	if err != nil {
		t.Fatalf("NewNodeKey: %v", err)
	}
	if err := devicekeys.SaveNodeKey(paths.NodeKey, current); err != nil {
		t.Fatalf("SaveNodeKey: %v", err)
	}
	mk, err := devicekeys.NewMachineKey()
	if err != nil {
		t.Fatalf("NewMachineKey: %v", err)
	}

	f := &rotatorFixture{
		t:        t,
		stateDir: stateDir,
		paths:    paths,
		current:  current,
		// The device starts agreeing with the CP.
		cp: &fakeControlPlane{current: current.PublicBase64()},
	}
	f.published = current.PublicBase64()

	srv := f.cp.serve(t)
	f.rotator = newNodeKeyRotator(nodeKeyRotatorConfig{
		StateDir:       stateDir,
		ControlURL:     srv.URL,
		DeviceID:       "dev-test",
		NetworkID:      "net-test",
		MachineKey:     mk,
		CurrentNodeKey: current,
		PublishedNodeKey: func() string {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.published
		},
		TriggerReactivate: func() {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.reactivs++
		},
		BearerFn:      func() string { return "test-token" },
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		CheckInterval: 10 * time.Millisecond,
	})
	return f
}

// stage writes a fresh node.key.next and returns it.
func (f *rotatorFixture) stage() *devicekeys.NodeKey {
	f.t.Helper()
	k, err := devicekeys.NewNodeKey()
	if err != nil {
		f.t.Fatalf("NewNodeKey: %v", err)
	}
	if err := devicekeys.SaveNodeKey(f.paths.NodeKeyNext, k); err != nil {
		f.t.Fatalf("SaveNodeKey(next): %v", err)
	}
	return k
}

func (f *rotatorFixture) setPublished(k string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = k
}

func (f *rotatorFixture) reactivations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reactivs
}

// onDiskCurrent is the public half of whatever secrets/node.key now holds.
func (f *rotatorFixture) onDiskCurrent() string {
	f.t.Helper()
	k, err := devicekeys.LoadOrCreateNodeKey(f.paths.NodeKey)
	if err != nil {
		f.t.Fatalf("load node.key: %v", err)
	}
	return k.PublicBase64()
}

// stagedBytes is the raw content of node.key.next, or nil when absent.
func (f *rotatorFixture) stagedBytes() []byte {
	f.t.Helper()
	b, err := os.ReadFile(f.paths.NodeKeyNext)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		f.t.Fatalf("read node.key.next: %v", err)
	}
	return b
}

// TestRecoverStagedRotation_NothingStaged: with no node.key.next there is
// nothing to reconcile and the control plane is not contacted at all.
// Record of today's behaviour.
func TestRecoverStagedRotation_NothingStaged(t *testing.T) {
	f := newRotatorFixture(t)
	if got := f.rotator.recoverStagedRotation(context.Background()); got != recoveryNone {
		t.Fatalf("outcome = %v, want recoveryNone", got)
	}
	if n := len(f.cp.sent()); n != 0 {
		t.Fatalf("control plane got %d requests, want 0", n)
	}
}

// TestRecoverStagedRotation_PriorPostNeverCommitted: the CP still holds
// our on-disk key, so the interrupted rotation is simply finished — the
// staged key is promoted and the session re-activates. Record of today's
// behaviour (#228 crash-safety).
func TestRecoverStagedRotation_PriorPostNeverCommitted(t *testing.T) {
	f := newRotatorFixture(t)
	staged := f.stage()

	if got := f.rotator.recoverStagedRotation(context.Background()); got != recoveryDone {
		t.Fatalf("outcome = %v, want recoveryDone", got)
	}
	sent := f.cp.sent()
	if len(sent) != 1 {
		t.Fatalf("control plane got %d requests, want 1", len(sent))
	}
	if sent[0].Old != f.current.PublicBase64() || sent[0].New != staged.PublicBase64() {
		t.Fatalf("rotation posted old=%q new=%q, want old=%q new=%q",
			sent[0].Old, sent[0].New, f.current.PublicBase64(), staged.PublicBase64())
	}
	if sent[0].Sig == "" {
		t.Error("rotation posted an empty machine_signature")
	}
	if got := f.onDiskCurrent(); got != staged.PublicBase64() {
		t.Errorf("node.key = %q, want the staged key %q", got, staged.PublicBase64())
	}
	if f.stagedBytes() != nil {
		t.Error("node.key.next still present after a completed rotation")
	}
	if n := f.reactivations(); n != 1 {
		t.Errorf("reactivations = %d, want 1", n)
	}
}

// TestRecoverStagedRotation_AdoptsWhenMapConfirms: the prior POST did
// commit (the CP holds the staged key) and the network map says so, so
// the staged key is promoted. Product contract per waired-agent#729 —
// adoption is allowed only on this confirmed path.
func TestRecoverStagedRotation_AdoptsWhenMapConfirms(t *testing.T) {
	f := newRotatorFixture(t)
	staged := f.stage()
	// Model the lost promote: the CP committed the staged key, the map
	// publishes it, but node.key on disk is still the old one.
	f.cp.current = staged.PublicBase64()
	f.setPublished(staged.PublicBase64())

	if got := f.rotator.recoverStagedRotation(context.Background()); got != recoveryDone {
		t.Fatalf("outcome = %v, want recoveryDone", got)
	}
	if got := f.onDiskCurrent(); got != staged.PublicBase64() {
		t.Errorf("node.key = %q, want the staged key %q", got, staged.PublicBase64())
	}
	if f.stagedBytes() != nil {
		t.Error("node.key.next still present after adoption")
	}
	if n := f.reactivations(); n != 1 {
		t.Errorf("reactivations = %d, want 1", n)
	}
}

// TestRecoverStagedRotation_DoesNotAdoptSomeoneElsesKey: the CP holds a
// third key — the shape a re-authentication leaves behind now that the
// control plane also writes node_public_key (waired-ai/waired#1181). The
// mismatch error looks exactly like the "our POST committed" case, so
// only the map tells them apart. Product contract per waired-agent#729:
// the staged key must NOT be promoted here.
func TestRecoverStagedRotation_DoesNotAdoptSomeoneElsesKey(t *testing.T) {
	f := newRotatorFixture(t)
	staged := f.stage()
	third, err := devicekeys.NewNodeKey()
	if err != nil {
		t.Fatalf("NewNodeKey: %v", err)
	}
	f.cp.current = third.PublicBase64()
	f.setPublished(third.PublicBase64())

	stagedBefore := f.stagedBytes()
	if got := f.rotator.recoverStagedRotation(context.Background()); got != recoveryPending {
		t.Fatalf("outcome = %v, want recoveryPending", got)
	}
	if got := f.onDiskCurrent(); got != f.current.PublicBase64() {
		t.Errorf("node.key = %q, want it left at %q", got, f.current.PublicBase64())
	}
	if !bytes.Equal(f.stagedBytes(), stagedBefore) {
		t.Error("node.key.next changed; the staged key must be left for a later attempt")
	}
	if n := f.reactivations(); n != 0 {
		t.Errorf("reactivations = %d, want 0 — nothing was promoted", n)
	}
	if got := f.cp.held(); got != third.PublicBase64() {
		t.Errorf("control plane now holds %q, want it untouched at %q", got, third.PublicBase64())
	}
	// The attempt really did offer the staged key and really was refused:
	// without this the assertions above would also pass if the subject had
	// never contacted the control plane.
	sent := f.cp.sent()
	if len(sent) != 1 || sent[0].New != staged.PublicBase64() {
		t.Fatalf("control plane saw %+v, want one request offering the staged key %q",
			sent, staged.PublicBase64())
	}
}

// TestRecoverStagedRotation_DoesNotAdoptWithoutAMap: before the first
// network map arrives there is nothing to verify against, so the staged
// key is left alone. Product contract per waired-agent#729 (the
// verification is the precondition for adopting); the resulting delay
// was accepted as the safe direction.
func TestRecoverStagedRotation_DoesNotAdoptWithoutAMap(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire func(*rotatorFixture)
	}{
		{"no map seen yet", func(f *rotatorFixture) { f.setPublished("") }},
		{"no accessor wired", func(f *rotatorFixture) { f.rotator.cfg.PublishedNodeKey = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotatorFixture(t)
			staged := f.stage()
			// The CP committed the staged key: adoption would be the
			// correct end state, and is still refused for want of proof.
			f.cp.current = staged.PublicBase64()
			tc.wire(f)

			if got := f.rotator.recoverStagedRotation(context.Background()); got != recoveryPending {
				t.Fatalf("outcome = %v, want recoveryPending", got)
			}
			if got := f.onDiskCurrent(); got != f.current.PublicBase64() {
				t.Errorf("node.key = %q, want it left at %q", got, f.current.PublicBase64())
			}
			if f.stagedBytes() == nil {
				t.Error("node.key.next was removed; it must survive for a later attempt")
			}
			if n := f.reactivations(); n != 0 {
				t.Errorf("reactivations = %d, want 0", n)
			}
		})
	}
}

// TestRecoverStagedRotation_AdoptsOnceTheMapCatchesUp: the deferral is a
// delay, not a dead end — the same rotator adopts on a later attempt
// once the map publishes the staged key.
func TestRecoverStagedRotation_AdoptsOnceTheMapCatchesUp(t *testing.T) {
	f := newRotatorFixture(t)
	staged := f.stage()
	f.cp.current = staged.PublicBase64()
	f.setPublished("")

	if got := f.rotator.recoverStagedRotation(context.Background()); got != recoveryPending {
		t.Fatalf("first attempt = %v, want recoveryPending", got)
	}
	f.setPublished(staged.PublicBase64())
	if got := f.rotator.recoverStagedRotation(context.Background()); got != recoveryDone {
		t.Fatalf("second attempt = %v, want recoveryDone", got)
	}
	if got := f.onDiskCurrent(); got != staged.PublicBase64() {
		t.Errorf("node.key = %q, want the staged key %q", got, staged.PublicBase64())
	}
}

// TestRun_UnreconciledStagedKeyBlocksScheduledRotation: the scheduled
// rotation writes node.key.next, so letting it run while a staged key is
// unreconciled would overwrite the only copy of a key the control plane
// may already hold. Run must keep retrying recovery instead. Record of
// today's behaviour introduced with waired-agent#729 — the deferral this
// PR adds is what makes the window reachable.
func TestRun_UnreconciledStagedKeyBlocksScheduledRotation(t *testing.T) {
	f := newRotatorFixture(t)
	staged := f.stage()
	third, err := devicekeys.NewNodeKey()
	if err != nil {
		t.Fatalf("NewNodeKey: %v", err)
	}
	f.cp.current = third.PublicBase64()
	f.setPublished(third.PublicBase64())
	// Due now: without the block, maybeRotate would generate a fresh key
	// and overwrite the staged one on the first tick.
	if err := identity.SaveNodeKeyMeta(f.stateDir, identity.NodeKeyMeta{
		IssuedAt:  time.Now().Add(-200 * 24 * time.Hour),
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("SaveNodeKeyMeta: %v", err)
	}

	stagedBefore := f.stagedBytes()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.rotator.Run(ctx) }()

	// Let several CheckIntervals elapse so a clobber would have happened.
	waitFor(t, func() bool { return len(f.cp.sent()) >= 3 }, "three recovery attempts")
	cancel()
	<-done

	if !bytes.Equal(f.stagedBytes(), stagedBefore) {
		t.Error("node.key.next was overwritten while it was still unreconciled")
	}
	if got := f.onDiskCurrent(); got != f.current.PublicBase64() {
		t.Errorf("node.key = %q, want it left at %q", got, f.current.PublicBase64())
	}
	for i, req := range f.cp.sent() {
		if req.New != staged.PublicBase64() {
			t.Fatalf("request %d posted new=%q, want only the staged key %q — a new rotation was started",
				i, req.New, staged.PublicBase64())
		}
	}
	if n := f.reactivations(); n != 0 {
		t.Errorf("reactivations = %d, want 0", n)
	}
}

// TestPublishedNodeKeyIsStdBase64 pins the two sides of the comparison to
// the same encoding: the map's self row and NodeKey.PublicBase64() are
// both std-base64 of the raw 32-byte X25519 public key, so string
// equality is a valid key comparison.
func TestPublishedNodeKeyIsStdBase64(t *testing.T) {
	k, err := devicekeys.NewNodeKey()
	if err != nil {
		t.Fatalf("NewNodeKey: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(k.PublicBase64())
	if err != nil {
		t.Fatalf("PublicBase64 is not std-base64: %v", err)
	}
	if !bytes.Equal(raw, k.Public[:]) {
		t.Fatal("PublicBase64 does not round-trip to the raw public key")
	}
	p := &agentProvider{}
	if got := p.PublishedNodeKey(); got != "" {
		t.Errorf("PublishedNodeKey() before any map = %q, want empty", got)
	}
	p.publishedNodePub = k.PublicBase64()
	if got := p.PublishedNodeKey(); got != k.PublicBase64() {
		t.Errorf("PublishedNodeKey() = %q, want %q", got, k.PublicBase64())
	}
}

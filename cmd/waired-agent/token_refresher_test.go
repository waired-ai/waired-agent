package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/controlclient"
	"github.com/waired-ai/waired-agent/internal/devicekeys"
	"github.com/waired-ai/waired-agent/internal/identity"
)

// refreshProbe is a Control-Plane stub that records the client_nonce of
// every rotation it receives and answers each one from a scripted list.
// It records the real request body (not a summary) so a test can assert
// on what the agent actually put on the wire.
type refreshProbe struct {
	mu      sync.Mutex
	nonces  []string
	replies []func(w http.ResponseWriter)
}

func (p *refreshProbe) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ClientNonce string `json:"client_nonce"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode refresh body: %v", err)
		}
		p.mu.Lock()
		n := len(p.nonces)
		p.nonces = append(p.nonces, req.ClientNonce)
		p.mu.Unlock()
		if n < len(p.replies) {
			p.replies[n](w)
			return
		}
		t.Errorf("unexpected refresh attempt #%d", n+1)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func (p *refreshProbe) seen() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.nonces...)
}

// replyHangUp closes the connection without answering — the shape of the
// real incident, where the rotation committed server-side but the reply
// never reached the agent.
func replyHangUp(t *testing.T) func(http.ResponseWriter) {
	t.Helper()
	return func(w http.ResponseWriter) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("ResponseWriter is not a Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close()
	}
}

func replyRotated(access, refresh string, expiresAt time.Time) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		json.NewEncoder(w).Encode(map[string]any{
			"device_access_token":            access,
			"device_access_token_expires_at": expiresAt.UTC().Format(time.RFC3339),
			"device_refresh_token":           refresh,
		})
	}
}

func reply401(errType string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{"type": errType},
		})
	}
}

func testMachineKey(t *testing.T) *devicekeys.MachineKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	return &devicekeys.MachineKey{Public: pub, Private: priv}
}

func probeRefresher(t *testing.T, url string, cfg tokenRefresherConfig) *tokenRefresher {
	t.Helper()
	cfg.StateDir = t.TempDir()
	cfg.ControlURL = url
	cfg.DeviceID = "dev_TEST"
	cfg.NetworkID = "wn_TEST"
	cfg.MachineKey = testMachineKey(t)
	if cfg.InitialAccess == "" {
		cfg.InitialAccess = "waired_dat_initial"
	}
	if cfg.InitialRefresh == "" {
		cfg.InitialRefresh = "waired_drt_initial"
	}
	if cfg.RetryDelay == 0 {
		cfg.RetryDelay = time.Millisecond
	}
	return newTokenRefresher(cfg)
}

// TestTokenRefresherReplaysLostRotation is the regression test for the
// live lockout in waired-agent#318: a rotation whose response never
// arrived must be retried as the SAME request. Presenting the same
// refresh token under a fresh nonce is what the Control Plane cannot
// tell apart from a stolen token, and answering that is what flipped a
// working device to reauth_required.
func TestTokenRefresherReplaysLostRotation(t *testing.T) {
	probe := &refreshProbe{replies: []func(http.ResponseWriter){
		replyHangUp(t),
		replyRotated("waired_dat_new", "waired_drt_new", time.Now().Add(15*time.Minute)),
	}}
	srv := httptest.NewServer(probe.handler(t))
	defer srv.Close()

	r := probeRefresher(t, srv.URL, tokenRefresherConfig{})
	if err := r.refreshOnce(context.Background()); err != nil {
		t.Fatalf("refreshOnce: %v", err)
	}

	seen := probe.seen()
	if len(seen) != 2 {
		t.Fatalf("expected 2 attempts, got %d (%v)", len(seen), seen)
	}
	if seen[0] == "" {
		t.Fatal("first attempt carried no client_nonce")
	}
	if seen[0] != seen[1] {
		t.Fatalf("retry must replay the same nonce: first %q, retry %q", seen[0], seen[1])
	}
	if got := r.Get(); got != "waired_dat_new" {
		t.Fatalf("access token = %q, want the rotated one", got)
	}
	if r.pending != nil {
		t.Fatal("a successful rotation must clear the pending replay")
	}
}

// TestTokenRefresherDropsPendingOnVerdict pins the other side: any
// classified answer is the CP speaking, so there is nothing left to
// replay and the next attempt starts a fresh rotation.
func TestTokenRefresherDropsPendingOnVerdict(t *testing.T) {
	probe := &refreshProbe{replies: []func(http.ResponseWriter){
		replyHangUp(t),
		reply401("device_suspended"),
	}}
	srv := httptest.NewServer(probe.handler(t))
	defer srv.Close()

	r := probeRefresher(t, srv.URL, tokenRefresherConfig{})
	if err := r.refreshOnce(context.Background()); !errors.Is(err, controlclient.ErrDeviceSuspended) {
		t.Fatalf("refreshOnce = %v, want ErrDeviceSuspended", err)
	}
	if r.pending != nil {
		t.Fatal("a classified 401 must clear the pending replay")
	}
}

// TestTokenRefresherStopsReplayingPastWindow pins the safety valve: past
// the window the Control Plane can no longer match the replay to the
// rotation whose reply it lost, so replaying would knowingly trip reuse
// detection. Start a fresh rotation instead.
func TestTokenRefresherStopsReplayingPastWindow(t *testing.T) {
	r := probeRefresher(t, "http://cp", tokenRefresherConfig{ReplayWindow: time.Minute})
	now := time.Date(2026, 8, 1, 6, 25, 0, 0, time.UTC)
	r.pending = &pendingRotation{nonce: "pinned", refreshToken: "waired_drt_initial", at: now}

	if got, replayed := r.rotationNonce("waired_drt_initial", now.Add(30*time.Second)); !replayed || got != "pinned" {
		t.Fatalf("inside the window: got (%q, %v), want (\"pinned\", true)", got, replayed)
	}
	if got, replayed := r.rotationNonce("waired_drt_initial", now.Add(90*time.Second)); replayed || got == "pinned" {
		t.Fatalf("past the window: got (%q, %v), want a fresh nonce", got, replayed)
	}
	if r.pending != nil {
		t.Fatal("an expired pending rotation must be dropped")
	}
}

// TestTokenRefresherStopsReplayingAfterTokenRotated covers the other
// invalidation: if the stored refresh token changed, a later attempt
// already succeeded and the pending nonce belongs to a dead credential.
func TestTokenRefresherStopsReplayingAfterTokenRotated(t *testing.T) {
	r := probeRefresher(t, "http://cp", tokenRefresherConfig{})
	now := time.Date(2026, 8, 1, 6, 25, 0, 0, time.UTC)
	r.pending = &pendingRotation{nonce: "pinned", refreshToken: "waired_drt_old", at: now}

	got, replayed := r.rotationNonce("waired_drt_new", now)
	if replayed || got == "pinned" {
		t.Fatalf("got (%q, %v), want a fresh nonce", got, replayed)
	}
	if r.pending != nil {
		t.Fatal("a stale pending rotation must be dropped")
	}
}

// TestTokenRefresherTerminalFiresOnTerminal pins the quiesce hook: when
// auto-refresh gives up, the daemon has to be told, or every CP-facing
// loop keeps spending a bearer that will never be renewed (the live
// incident logged ~107k 401s that way).
func TestTokenRefresherTerminalFiresOnTerminal(t *testing.T) {
	probe := &refreshProbe{replies: []func(http.ResponseWriter){
		reply401("refresh_token_reuse_detected"),
	}}
	srv := httptest.NewServer(probe.handler(t))
	defer srv.Close()

	var mu sync.Mutex
	var causes []error
	r := probeRefresher(t, srv.URL, tokenRefresherConfig{
		InitialMeta: identity.TokenMeta{AccessExpiresAt: time.Now().Add(-time.Hour)},
		OnTerminal: func(err error) {
			mu.Lock()
			defer mu.Unlock()
			causes = append(causes, err)
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), waitBackstop)
	defer cancel()
	r.Run(ctx) // returns on the terminal classification, not on ctx

	if ctx.Err() != nil {
		t.Fatal("Run must return on a terminal error, not wait for ctx")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(causes) != 1 {
		t.Fatalf("OnTerminal fired %d times, want exactly 1", len(causes))
	}
	if !errors.Is(causes[0], controlclient.ErrRefreshReuseDetected) {
		t.Fatalf("OnTerminal cause = %v, want ErrRefreshReuseDetected", causes[0])
	}
	meta, err := identity.LoadTokenMeta(r.stateDir)
	if err != nil {
		t.Fatalf("LoadTokenMeta: %v", err)
	}
	if !meta.NeedsReauth() {
		t.Fatal("the terminal path must persist the reauth_required flag")
	}
}

// TestTokenRefresherNextSleep pins the product contract from
// waired-agent#318: once the refresh point has passed there is no delay
// left to serve — renew now. This test previously asserted the opposite
// (a minSleep floor in all three of those cases), and that floor was the
// ~30 seconds a cold-booted host spent 401-ing the Control Plane with an
// access token that had expired hours before. Inverted deliberately.
func TestTokenRefresherNextSleep(t *testing.T) {
	now := time.Date(2026, 5, 19, 3, 0, 0, 0, time.UTC)

	cases := []struct {
		name   string
		access time.Time
		want   time.Duration
	}{
		{"zero expiry → refresh now", time.Time{}, 0},
		{"expiry far in future → expiry - lead", now.Add(15 * time.Minute), 13 * time.Minute},
		{"expiry inside lead → refresh now", now.Add(30 * time.Second), 0},
		{"expired → refresh now", now.Add(-1 * time.Hour), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTokenRefresher(tokenRefresherConfig{
				StateDir:       t.TempDir(),
				ControlURL:     "http://cp",
				DeviceID:       "d",
				NetworkID:      "n",
				InitialAccess:  "tok",
				InitialRefresh: "r",
				InitialMeta:    identity.TokenMeta{AccessExpiresAt: tc.access},
			})
			if got := r.nextSleep(now); got != tc.want {
				t.Fatalf("nextSleep = %v, want %v", got, tc.want)
			}
			// accessTokenStale is the gate the daemon uses to decide
			// whether to renew before handing the bearer to the CP push
			// loops; it must agree with nextSleep.
			if got, want := r.accessTokenStale(now), tc.want == 0; got != want {
				t.Fatalf("accessTokenStale = %v, want %v", got, want)
			}
		})
	}
}

// TestTokenRefresherAccessTokenStaleNoToken covers the branch nextSleep
// cannot see: a valid-looking expiry with no token behind it.
func TestTokenRefresherAccessTokenStaleNoToken(t *testing.T) {
	now := time.Date(2026, 5, 19, 3, 0, 0, 0, time.UTC)
	r := newTokenRefresher(tokenRefresherConfig{
		StateDir:       t.TempDir(),
		ControlURL:     "http://cp",
		DeviceID:       "d",
		NetworkID:      "n",
		InitialRefresh: "r",
		InitialMeta:    identity.TokenMeta{AccessExpiresAt: now.Add(time.Hour)},
	})
	if !r.accessTokenStale(now) {
		t.Fatal("an empty access token must read as stale")
	}
}

func TestTokenRefresherCanRefresh(t *testing.T) {
	cases := []struct {
		name string
		cfg  tokenRefresherConfig
		want bool
	}{
		{
			name: "happy",
			cfg: tokenRefresherConfig{
				StateDir:       t.TempDir(),
				ControlURL:     "http://cp",
				DeviceID:       "d",
				NetworkID:      "n",
				MachineKey:     &devicekeys.MachineKey{},
				InitialRefresh: "r",
			},
			want: true,
		},
		{"no refresh", tokenRefresherConfig{StateDir: t.TempDir(), ControlURL: "x", DeviceID: "d", NetworkID: "n", MachineKey: &devicekeys.MachineKey{}}, false},
		{"no machine key", tokenRefresherConfig{StateDir: t.TempDir(), ControlURL: "x", DeviceID: "d", NetworkID: "n", InitialRefresh: "r"}, false},
		{"no control url", tokenRefresherConfig{StateDir: t.TempDir(), DeviceID: "d", NetworkID: "n", MachineKey: &devicekeys.MachineKey{}, InitialRefresh: "r"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTokenRefresher(tc.cfg)
			if got := r.canRefresh(); got != tc.want {
				t.Fatalf("canRefresh = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestTokenRefresherMarkReauthRequiredPersistsMeta covers the #115
// Phase C flag-persistence path: when the refresh loop classifies a
// terminal auth failure, markReauthRequired must write the on-disk
// flag so subsequent `waired auth status` (and any future tray /
// web-admin surface) can tell the operator what happened.
func TestTokenRefresherMarkReauthRequiredPersistsMeta(t *testing.T) {
	dir := t.TempDir()
	access := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	auth := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	r := newTokenRefresher(tokenRefresherConfig{
		StateDir:       dir,
		ControlURL:     "http://cp",
		DeviceID:       "d",
		NetworkID:      "n",
		MachineKey:     &devicekeys.MachineKey{},
		InitialAccess:  "tok",
		InitialRefresh: "r",
		InitialMeta:    identity.TokenMeta{AccessExpiresAt: access, DeviceAuthExpiresAt: auth},
	})

	r.markReauthRequired(errors.New("cause"))

	got, err := identity.LoadTokenMeta(dir)
	if err != nil {
		t.Fatalf("LoadTokenMeta: %v", err)
	}
	if !got.NeedsReauth() {
		t.Fatalf("expected NeedsReauth==true after markReauthRequired")
	}
	if !got.AccessExpiresAt.Equal(access) || !got.DeviceAuthExpiresAt.Equal(auth) {
		t.Fatalf("markReauthRequired must preserve existing expiries; got %+v", got)
	}
}

// TestTokenRefresherReplaysWhenPersistFails pins the other way a
// rotation can be lost: the Control Plane rotated and answered, but we
// could not write the new pair to disk. The agent is then holding a
// burned credential exactly as if the response had never arrived, so
// the next attempt has to replay the same request rather than mint a
// new rotation the CP would read as theft.
func TestTokenRefresherReplaysWhenPersistFails(t *testing.T) {
	probe := &refreshProbe{replies: []func(http.ResponseWriter){
		replyRotated("waired_dat_1", "waired_drt_1", time.Now().Add(15*time.Minute)),
		replyRotated("waired_dat_2", "waired_drt_2", time.Now().Add(15*time.Minute)),
	}}
	srv := httptest.NewServer(probe.handler(t))
	defer srv.Close()

	r := probeRefresher(t, srv.URL, tokenRefresherConfig{})
	// Make the persist step fail. The state dir is created on demand, so
	// point it *through* a regular file: every mkdir below it fails with
	// ENOTDIR. The rotation itself still succeeds server-side, which is
	// the situation under test.
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	r.stateDir = filepath.Join(blocked, "state")

	if err := r.refreshOnce(context.Background()); err == nil {
		t.Fatal("expected the persist failure to surface")
	}
	if r.pending == nil {
		t.Fatal("a rotation the CP committed but we could not store must stay replayable")
	}
	firstNonce := probe.seen()[0]
	if r.pending.nonce != firstNonce {
		t.Fatalf("pending nonce %q, want the one presented %q", r.pending.nonce, firstNonce)
	}
	// The stored refresh token must NOT have advanced — that is what
	// makes the pending attempt still match on the retry.
	if got := r.pending.refreshToken; got != "waired_drt_initial" {
		t.Fatalf("pending refreshToken = %q, want the credential still held", got)
	}

	// Next attempt replays the same request.
	r.stateDir = t.TempDir()
	if err := r.refreshOnce(context.Background()); err != nil {
		t.Fatalf("retry: %v", err)
	}
	seen := probe.seen()
	if len(seen) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(seen))
	}
	if seen[0] != seen[1] {
		t.Fatalf("retry must replay the same nonce: %q vs %q", seen[0], seen[1])
	}
	if r.pending != nil {
		t.Fatal("a completed rotation must clear the pending replay")
	}
}

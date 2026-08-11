package wgnet_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/network/wgnet"
)

// TestForeignKeyHandshakeNamesItsSender drives the condition
// waired-agent#712 is about through two real userspace WireGuard
// engines: a sender that holds the wrong static public key for the
// receiver.
//
// The mac1 arithmetic under test has to agree with wireguard-go's, and
// no assertion built from the same code could show that. So the message
// here is produced by wireguard-go itself, and the receiver is a real
// Engine — the seam is the receiver's log, below everything the fix
// touches.
func TestForeignKeyHandshakeNamesItsSender(t *testing.T) {
	cases := []struct {
		name       string
		wrongKey   bool
		wantNamed  bool
		wantSilent bool
		// budget bounds the handshake loop. The named case exits as soon
		// as the report lands; the silent case has to spend its whole
		// budget proving a negative, so it gets the smaller one.
		budget time.Duration
	}{
		{
			name:      "a peer using the wrong public key for us is named",
			wrongKey:  true,
			wantNamed: true,
			budget:    10 * time.Second,
		},
		{
			// The receiver of a correct handshake must stay silent, or
			// the warning means nothing.
			name:       "a peer using the right public key for us is not",
			wrongKey:   false,
			wantSilent: true,
			budget:     2 * time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			alicePriv, alicePub := genKey(t)
			bobPriv, bobPub := genKey(t)
			_, strangerPub := genKey(t)

			aliceIP := netip.MustParseAddr("100.96.0.10")
			bobIP := netip.MustParseAddr("100.96.0.11")

			bobLogs := &syncBuffer{}
			bobEng, err := wgnet.NewEngine(wgnet.Config{
				SelfName:       "bob",
				SelfOverlayIP:  bobIP,
				SelfPrivateKey: bobPriv,
				SelfDeviceID:   "dev_bob",
				SelfNodePub:    base64.StdEncoding.EncodeToString(bobPub),
				Logger:         slog.New(slog.NewTextHandler(bobLogs, &slog.HandlerOptions{Level: slog.LevelWarn})),
			})
			if err != nil {
				t.Fatalf("bob NewEngine: %v", err)
			}
			defer bobEng.Close()
			bobPort, err := bobEng.ListenPort()
			if err != nil {
				t.Fatalf("bob ListenPort: %v", err)
			}

			bobKeyAsAliceSeesIt := bobPub
			if tc.wrongKey {
				bobKeyAsAliceSeesIt = strangerPub
			}
			aliceEng, err := wgnet.NewEngine(wgnet.Config{
				SelfName:       "alice",
				SelfOverlayIP:  aliceIP,
				SelfPrivateKey: alicePriv,
				SelfDeviceID:   "dev_alice",
				SelfNodePub:    base64.StdEncoding.EncodeToString(alicePub),
				Logger:         slog.New(slog.NewTextHandler(&syncBuffer{}, &slog.HandlerOptions{Level: slog.LevelError})),
				Peers: []wgnet.Peer{{
					DeviceName:          "bob",
					OverlayIP:           bobIP,
					WireGuardPublicKey:  bobKeyAsAliceSeesIt,
					Endpoint:            netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(bobPort)).String(),
					PersistentKeepalive: 5,
				}},
			})
			if err != nil {
				t.Fatalf("alice NewEngine: %v", err)
			}
			defer aliceEng.Close()

			// Bob learns alice only now: each engine needs the other's
			// bound port, and only one of them can be built first.
			alicePort, err := aliceEng.ListenPort()
			if err != nil {
				t.Fatalf("alice ListenPort: %v", err)
			}
			if err := bobEng.UpdatePeers([]wgnet.Peer{{
				DeviceName:         "alice",
				OverlayIP:          aliceIP,
				WireGuardPublicKey: alicePub,
				Endpoint:           netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(alicePort)).String(),
			}}); err != nil {
				t.Fatalf("bob UpdatePeers: %v", err)
			}

			// A queued packet is what makes wireguard-go send a handshake
			// initiation. It never completes in the wrong-key case, so the
			// dial is expected to fail either way.
			dial := func() {
				ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
				defer cancel()
				if c, derr := aliceEng.DialOverlayTCP(ctx, bobIP, 9474); derr == nil {
					c.Close()
				}
			}

			const want = "handshakes authenticated against a public key this device does not hold"
			start := time.Now()
			deadline := start.Add(tc.budget)
			for time.Now().Before(deadline) {
				dial()
				if strings.Contains(bobLogs.String(), want) {
					t.Logf("saw the report after %s", time.Since(start))
					break
				}
			}

			got := bobLogs.String()
			if tc.wantNamed {
				if !strings.Contains(got, want) {
					t.Fatalf("bob never reported the foreign key; log:\n%s", got)
				}
				if !strings.Contains(got, "127.0.0.1:") {
					t.Fatalf("report does not name the sender's address; log:\n%s", got)
				}
			}
			if tc.wantSilent && strings.Contains(got, want) {
				t.Fatalf("bob reported a foreign key for a correct handshake; log:\n%s", got)
			}
		})
	}
}

// syncBuffer is a log sink the test goroutine can read while the
// engine's receive goroutines are writing to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

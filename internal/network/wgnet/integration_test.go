package wgnet_test

import (
	"context"
	"crypto/rand"
	"net/netip"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/curve25519"

	"github.com/waired-ai/waired-agent/internal/inference"
	"github.com/waired-ai/waired-agent/internal/network/wgnet"
)

// TestTwoEnginesPing brings up two userspace WireGuard engines on real
// loopback UDP ports in the same process and asserts that an overlay HTTP
// ping issued from alice reaches bob's inference handler. This is the
// "two waired daemons in one process" integration described in
// docs/specs/waired_client_network_spec.md §33.2.
func TestTwoEnginesPing(t *testing.T) {
	alicePriv, alicePub := genKey(t)
	bobPriv, bobPub := genKey(t)

	const (
		alicePort = 41820
		bobPort   = 41821
		svcPort   = 9474
	)
	aliceIP := netip.MustParseAddr("100.96.0.10")
	bobIP := netip.MustParseAddr("100.96.0.11")

	aliceEng, err := wgnet.NewEngine(wgnet.Config{
		SelfName:       "alice",
		SelfOverlayIP:  aliceIP,
		SelfPrivateKey: alicePriv,
		ListenPort:     alicePort,
		Peers: []wgnet.Peer{{
			DeviceName:          "bob",
			OverlayIP:           bobIP,
			WireGuardPublicKey:  bobPub,
			Endpoint:            "127.0.0.1:41821",
			PersistentKeepalive: 5,
		}},
	})
	if err != nil {
		t.Fatalf("alice NewEngine: %v", err)
	}
	defer aliceEng.Close()

	bobEng, err := wgnet.NewEngine(wgnet.Config{
		SelfName:       "bob",
		SelfOverlayIP:  bobIP,
		SelfPrivateKey: bobPriv,
		ListenPort:     bobPort,
		Peers: []wgnet.Peer{{
			DeviceName:          "alice",
			OverlayIP:           aliceIP,
			WireGuardPublicKey:  alicePub,
			Endpoint:            "127.0.0.1:41820",
			PersistentKeepalive: 5,
		}},
	})
	if err != nil {
		t.Fatalf("bob NewEngine: %v", err)
	}
	defer bobEng.Close()

	bobLn, err := bobEng.ListenOverlayTCP(svcPort)
	if err != nil {
		t.Fatalf("bob listen: %v", err)
	}
	defer bobLn.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bobSrv := inference.NewServer("bob")
	var serveErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		serveErr = bobSrv.ServeOverlay(ctx, bobLn)
	}()

	client := inference.NewClient(aliceEng, 10*time.Second)

	deadline := time.Now().Add(15 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
		body, latency, err := client.Ping(callCtx, bobIP, svcPort)
		callCancel()
		if err == nil {
			if !body.OK || body.Device != "bob" {
				t.Fatalf("unexpected ping body: %+v", body)
			}
			t.Logf("alice→bob ping ok latency=%s", latency)
			cancel()
			wg.Wait()
			if serveErr != nil {
				t.Fatalf("server error: %v", serveErr)
			}
			return
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	cancel()
	wg.Wait()
	t.Fatalf("ping never succeeded: %v", lastErr)
}

func genKey(t *testing.T) (priv, pub []byte) {
	t.Helper()
	priv = make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		t.Fatalf("rand: %v", err)
	}
	priv[0] &= 248
	priv[31] &= 127
	priv[31] |= 64
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		t.Fatalf("x25519: %v", err)
	}
	return priv, pub
}

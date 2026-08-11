package disco

import (
	"bytes"
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// lockedBuffer is a log sink the test goroutine can read while the
// service's inbound goroutine is still writing to it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestInboundSource pins the source label for every arrival shape.
//
// This is a fix pin, not a record of today's behaviour: waired-agent#712
// reports 737 relay-path decode failures logged as
// src="invalid AddrPort", and the "" rows below are what produced that
// string.
func TestInboundSource(t *testing.T) {
	addr := netip.MustParseAddrPort("203.0.113.9:51820")

	cases := []struct {
		name     string
		pkt      wireframe.Inbound
		peerName string
		want     string
	}{
		{
			name: "direct UDP is named by its source address",
			pkt:  wireframe.Inbound{Path: wireframe.PathDirect, Src: addr},
			want: "203.0.113.9:51820",
		},
		{
			name: "an untagged path is the direct path",
			pkt:  wireframe.Inbound{Src: addr},
			want: "203.0.113.9:51820",
		},
		{
			name: "a direct frame with no source address says so",
			pkt:  wireframe.Inbound{Path: wireframe.PathDirect},
			want: "unknown source",
		},
		{
			name:     "a relay frame is named by its peer, not its (absent) address",
			pkt:      wireframe.Inbound{Path: wireframe.PathRelay, RelaySrcDeviceID: "dev_peer", RelayURL: "wss://r1.example/relay/v1/connect"},
			peerName: "workshop-mac",
			want:     "workshop-mac via relay",
		},
		{
			name: "a relay frame from a device we do not know falls back to its id",
			pkt:  wireframe.Inbound{Path: wireframe.PathRelay, RelaySrcDeviceID: "dev_stranger"},
			want: "dev_stranger via relay",
		},
		{
			name: "a relay frame with no sender at all is still not an address",
			pkt:  wireframe.Inbound{Path: wireframe.PathRelay},
			want: "unidentified sender via relay",
		},
		{
			name:     "the relay path never reports a source address it did not receive",
			pkt:      wireframe.Inbound{Path: wireframe.PathRelay, Src: addr, RelaySrcDeviceID: "dev_peer"},
			peerName: "workshop-mac",
			want:     "workshop-mac via relay",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inboundSource(tc.pkt, tc.peerName); got != tc.want {
				t.Fatalf("inboundSource = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSrcOf_ResolvesTheLivePeerSet covers the lookup half: inboundSource
// is only as good as the name handed to it, and srcOf is the only
// production caller.
func TestSrcOf_ResolvesTheLivePeerSet(t *testing.T) {
	s, _, _ := newService(t, nil, newFakeBind())
	_, peerPub := newNodeKey(t)
	s.UpdatePeers(map[string]PeerSnapshot{
		encodeNodePubB64(peerPub): {DeviceID: "dev_peer", LogName: "workshop-mac", NodePub: peerPub},
	})

	got := s.srcOf(wireframe.Inbound{Path: wireframe.PathRelay, RelaySrcDeviceID: "dev_peer"})
	if want := "workshop-mac via relay"; got != want {
		t.Fatalf("srcOf(known peer) = %q, want %q", got, want)
	}

	got = s.srcOf(wireframe.Inbound{Path: wireframe.PathRelay, RelaySrcDeviceID: "dev_other"})
	if want := "dev_other via relay"; got != want {
		t.Fatalf("srcOf(unknown peer) = %q, want %q", got, want)
	}
}

// TestSealedDecodeFailureNamesTheSender drives the failure #712 is about
// through the real inbound loop: a relay-tunnelled frame sealed to the
// wrong key. The log line must identify the sender.
//
// The seam is the log handler, not a stubbed source function, so a
// handler that stopped calling srcOf would fail here.
func TestSealedDecodeFailureNamesTheSender(t *testing.T) {
	logs := &lockedBuffer{}
	bind := newFakeBind()
	selfPriv, selfPub := newNodeKey(t)
	s, err := New(Config{
		SelfDeviceID:       "dev_self",
		SelfNodeKeyPriv:    selfPriv,
		SelfNodeKeyPub:     selfPub,
		Bind:               bind,
		Logger:             slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		STUNObserveActive:  time.Hour,
		STUNTimeout:        200 * time.Millisecond,
		ProbeReprobeActive: time.Hour,
		ProbeWindow:        5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	senderPriv, senderPub := newNodeKey(t)
	_, strangerPub := newNodeKey(t) // not us: the AEAD will not open
	s.UpdatePeers(map[string]PeerSnapshot{
		encodeNodePubB64(senderPub): {DeviceID: "dev_peer", LogName: "workshop-mac", NodePub: senderPub},
	})

	sealed := mustEncodeSealed(t, &wireframe.Frame{
		Type:        wireframe.TypeProbe,
		SrcDeviceID: "dev_peer",
		DstDeviceID: "dev_self",
		HasNonce:    true,
	}, senderPriv, senderPub, strangerPub)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	bind.inbound <- wireframe.Inbound{
		Payload:          sealed,
		Path:             wireframe.PathRelay,
		RelayURL:         "wss://r1.example/relay/v1/connect",
		RelaySrcDeviceID: "dev_peer",
	}

	deadline := time.After(2 * time.Second)
	for !strings.Contains(logs.String(), "disco sealed decode") {
		select {
		case <-deadline:
			t.Fatalf("no sealed-decode log within 2s; got:\n%s", logs.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	line := logs.String()
	if !strings.Contains(line, `src="workshop-mac via relay"`) {
		t.Fatalf("sealed-decode log does not name the sender:\n%s", line)
	}
	if strings.Contains(line, "invalid AddrPort") {
		t.Fatalf("sealed-decode log still reports the zero address:\n%s", line)
	}
}

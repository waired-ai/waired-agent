package wgnet

import (
	"encoding/base64"
	"testing"
	"time"

	"golang.org/x/crypto/blake2s"
)

// nodePubB64 makes a deterministic 32-byte stand-in for a device's
// static public key. Synthetic on purpose: this repo is public and no
// real device key belongs in a fixture.
func nodePubB64(seed byte) string {
	b := make([]byte, 32)
	for i := range b {
		b[i] = seed + byte(i)
	}
	return base64.StdEncoding.EncodeToString(b)
}

var (
	selfNodePubB64 = nodePubB64(1)
	// A key that is not this device's. Any 32 bytes will do — the point
	// is only that it differs from the one the checker is armed with.
	otherNodePubB64 = nodePubB64(101)
)

// wgHandshake builds a handshake message of the given type whose mac1 is
// computed over receiverPubB64, i.e. exactly what a peer holding that
// public key for us would put on the wire. Built with the same primitive
// the subject uses, so the interesting assertions below are the ones
// that pair a message keyed to one device with a checker armed for
// another.
func wgHandshake(t *testing.T, msgType byte, size int, receiverPubB64 string) []byte {
	t.Helper()
	msg := make([]byte, size)
	msg[0] = msgType
	for i := 4; i < size-2*wgMACSize; i++ {
		msg[i] = byte(i)
	}
	c := newMAC1Checker(receiverPubB64)
	if !c.armed {
		t.Fatalf("newMAC1Checker(%q) disarmed", receiverPubB64)
	}
	end := size - 2*wgMACSize
	mac, err := blake2s.New128(c.key[:])
	if err != nil {
		t.Fatalf("blake2s.New128: %v", err)
	}
	mac.Write(msg[:end])
	mac.Sum(msg[end : end : end+wgMACSize])
	return msg
}

// TestMAC1Checker pins which inbound packets the checker will and will
// not accuse.
//
// A fix pin, not a record of today's behaviour: waired-agent#712 is that
// `Received packet with invalid mac1` names no sender, and this is the
// verdict that lets the bind name one.
func TestMAC1Checker(t *testing.T) {
	c := newMAC1Checker(selfNodePubB64)

	cases := []struct {
		name               string
		msg                []byte
		elsewhere, checked bool
	}{
		{
			name:      "an initiation addressed to us is ours",
			msg:       wgHandshake(t, wgMsgTypeInitiation, wgInitiationSize, selfNodePubB64),
			elsewhere: false, checked: true,
		},
		{
			name:      "an initiation addressed to another key is not",
			msg:       wgHandshake(t, wgMsgTypeInitiation, wgInitiationSize, otherNodePubB64),
			elsewhere: true, checked: true,
		},
		{
			name:      "a response addressed to us is ours",
			msg:       wgHandshake(t, wgMsgTypeResponse, wgResponseSize, selfNodePubB64),
			elsewhere: false, checked: true,
		},
		{
			name:      "a response addressed to another key is not",
			msg:       wgHandshake(t, wgMsgTypeResponse, wgResponseSize, otherNodePubB64),
			elsewhere: true, checked: true,
		},
		{
			// Transport data carries no mac1 over our key. Reporting one
			// would accuse every peer that is working perfectly.
			name:    "transport data is not judged",
			msg:     append([]byte{4, 0, 0, 0}, make([]byte, 60)...),
			checked: false,
		},
		{
			name:    "a cookie reply is not judged",
			msg:     append([]byte{3, 0, 0, 0}, make([]byte, 60)...),
			checked: false,
		},
		{
			name:    "an initiation-typed packet of the wrong length is not judged",
			msg:     append([]byte{1, 0, 0, 0}, make([]byte, 60)...),
			checked: false,
		},
		{
			name:    "a type whose reserved bytes are set is not judged",
			msg:     append([]byte{1, 0, 1, 0}, make([]byte, wgInitiationSize-4)...),
			checked: false,
		},
		{
			name:    "a runt packet is not judged",
			msg:     []byte{1, 0},
			checked: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			elsewhere, checked := c.addressedElsewhere(tc.msg)
			if checked != tc.checked || elsewhere != tc.elsewhere {
				t.Fatalf("addressedElsewhere = (%v, %v), want (%v, %v)",
					elsewhere, checked, tc.elsewhere, tc.checked)
			}
		})
	}
}

// TestMAC1Checker_DisarmedNeverAccuses records that a bind built without
// a usable self key stays silent instead of reporting every peer.
func TestMAC1Checker_DisarmedNeverAccuses(t *testing.T) {
	for _, pub := range []string{"", "not-base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		c := newMAC1Checker(pub)
		if c.armed {
			t.Fatalf("newMAC1Checker(%q) armed", pub)
		}
		msg := wgHandshake(t, wgMsgTypeInitiation, wgInitiationSize, otherNodePubB64)
		if elsewhere, checked := c.addressedElsewhere(msg); elsewhere || checked {
			t.Fatalf("disarmed checker judged a packet: (%v, %v)", elsewhere, checked)
		}
	}
}

// TestForeignKeyReporter pins the rate limit: the first sighting is
// reported at once so a short burst is not lost, and everything inside
// the interval is folded into the next release.
func TestForeignKeyReporter(t *testing.T) {
	base := time.Unix(1700000000, 0)
	r := newForeignKeyReporter(time.Minute)

	got := r.record("a", base)
	if len(got) != 1 || got[0].Src != "a" || got[0].Count != 1 {
		t.Fatalf("first record = %+v, want one a×1", got)
	}

	for i := 0; i < 5; i++ {
		if out := r.record("a", base.Add(time.Duration(i)*time.Second)); out != nil {
			t.Fatalf("record inside the interval released %+v", out)
		}
	}
	if out := r.record("b", base.Add(30*time.Second)); out != nil {
		t.Fatalf("a second source inside the interval released %+v", out)
	}

	// The release reports what accumulated since the previous one, so
	// the "a" already reported above is not counted twice.
	got = r.record("b", base.Add(time.Minute))
	want := []foreignKeySource{{Src: "a", Count: 5}, {Src: "b", Count: 2}}
	if len(got) != len(want) {
		t.Fatalf("release = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("release[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Counts start over after a release rather than accumulating for the
	// life of the process.
	if out := r.record("a", base.Add(2*time.Minute)); len(out) != 1 || out[0].Count != 1 {
		t.Fatalf("after release, record = %+v, want one a×1", out)
	}
}

// TestForeignKeyReporter_BusiestSenderFirst pins the ordering: with more
// than one sender, the one flooding is the one to read first.
func TestForeignKeyReporter_BusiestSenderFirst(t *testing.T) {
	base := time.Unix(1700000000, 0)
	r := newForeignKeyReporter(time.Minute)
	r.record("quiet", base) // releases immediately, resetting counts
	for i := 0; i < 3; i++ {
		r.record("quiet", base.Add(time.Second))
	}
	for i := 0; i < 9; i++ {
		r.record("loud", base.Add(time.Second))
	}
	got := r.record("loud", base.Add(time.Minute))
	if len(got) != 2 || got[0].Src != "loud" || got[1].Src != "quiet" {
		t.Fatalf("release = %+v, want loud before quiet", got)
	}
}

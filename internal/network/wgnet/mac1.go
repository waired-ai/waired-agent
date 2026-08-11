package wgnet

import (
	"crypto/hmac"
	"encoding/base64"
	"sort"
	"sync"
	"time"

	"golang.org/x/crypto/blake2s"
)

// WireGuard handshake framing, as implemented by
// golang.zx2c4.com/wireguard/device. Repeated here because the upstream
// constants are unexported and this file only reads the two fields every
// implementation agrees on: the leading message type and the trailing
// mac1.
//
// A handshake message ends with mac1 (16 B) followed by mac2 (16 B), and
//
//	mac1 = BLAKE2s-128(key = BLAKE2s-256("mac1----" ‖ Spub_receiver),
//	                   msg[:len-32])
//
// The key is the *receiver's* static public key. That is what makes this
// check answer waired-agent#712: a handshake we cannot reproduce the
// mac1 of was addressed to a different public key than the one this
// device holds — "somebody is using a different public key for me" —
// and unlike wireguard-go's own `Received packet with invalid mac1`
// line, we still hold the source it came from.
const (
	wgLabelMAC1 = "mac1----"

	wgMsgTypeInitiation = 1
	wgMsgTypeResponse   = 2

	wgInitiationSize = 148
	wgResponseSize   = 92

	wgMACSize = blake2s.Size128 // 16
)

// mac1Checker reproduces the mac1 of handshake messages addressed to
// this device's static public key. The zero value never claims a
// message is foreign — a bind built without a usable self key simply
// stops reporting rather than accusing every peer.
type mac1Checker struct {
	key   [blake2s.Size]byte
	armed bool
}

// newMAC1Checker derives the mac1 key from the device's own static
// public key, given as std-base64 (the form the agent already carries
// as MultiplexBindConfig.SelfNodePub). An unset or unparseable key
// leaves the checker disarmed.
func newMAC1Checker(selfNodePubB64 string) mac1Checker {
	raw, err := base64.StdEncoding.DecodeString(selfNodePubB64)
	if err != nil || len(raw) != 32 {
		return mac1Checker{}
	}
	h, err := blake2s.New256(nil)
	if err != nil {
		return mac1Checker{}
	}
	h.Write([]byte(wgLabelMAC1))
	h.Write(raw)
	var c mac1Checker
	h.Sum(c.key[:0])
	c.armed = true
	return c
}

// addressedElsewhere reports whether msg is a WireGuard handshake whose
// mac1 does not match this device's static public key.
//
// The second return says whether the question applied at all. Transport
// data, cookie replies, disco frames and anything mis-sized carry no
// mac1 over our key, and a false verdict on those would be a lie rather
// than a silence — so they answer (false, false) and the caller counts
// nothing.
func (c mac1Checker) addressedElsewhere(msg []byte) (elsewhere, checked bool) {
	if !c.armed || len(msg) < 4 {
		return false, false
	}
	// The type is a little-endian uint32 whose upper 3 bytes are
	// reserved zeros.
	if msg[1] != 0 || msg[2] != 0 || msg[3] != 0 {
		return false, false
	}
	switch {
	case msg[0] == wgMsgTypeInitiation && len(msg) == wgInitiationSize:
	case msg[0] == wgMsgTypeResponse && len(msg) == wgResponseSize:
	default:
		return false, false
	}
	end := len(msg) - 2*wgMACSize
	mac, err := blake2s.New128(c.key[:])
	if err != nil {
		return false, false
	}
	mac.Write(msg[:end])
	var want [wgMACSize]byte
	mac.Sum(want[:0])
	return !hmac.Equal(want[:], msg[end:end+wgMACSize]), true
}

// foreignKeySource is one reported sender: where it came from and how
// many handshakes it addressed to a key we do not hold since the last
// report.
type foreignKeySource struct {
	Src   string
	Count int
}

// foreignKeyReporter aggregates mac1 mismatches per source and releases
// them at most once per interval.
//
// Rate limiting is the point: the frozen bundle in waired-agent#712 held
// 1302 of these in 98 minutes, and one line per packet is what made them
// unreadable. A source is named on its first sighting so a short burst
// is not lost, then at most once per interval afterwards.
type foreignKeyReporter struct {
	mu       sync.Mutex
	counts   map[string]int
	lastAt   time.Time
	interval time.Duration
}

func newForeignKeyReporter(interval time.Duration) *foreignKeyReporter {
	if interval <= 0 {
		interval = time.Minute
	}
	return &foreignKeyReporter{counts: map[string]int{}, interval: interval}
}

// record folds one mismatch in and returns the batch to log, or nil when
// it is not yet time. Sources come out sorted so a fleet of hosts
// produces comparable lines.
func (r *foreignKeyReporter) record(src string, now time.Time) []foreignKeySource {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[src]++
	if !r.lastAt.IsZero() && now.Sub(r.lastAt) < r.interval {
		return nil
	}
	r.lastAt = now
	out := make([]foreignKeySource, 0, len(r.counts))
	for s, n := range r.counts {
		out = append(out, foreignKeySource{Src: s, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Src < out[j].Src
	})
	clear(r.counts)
	return out
}

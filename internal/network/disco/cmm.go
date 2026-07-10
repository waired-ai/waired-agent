package disco

import (
	"encoding/base64"
	"errors"
	"net/netip"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// SendCallMeMaybe encodes a sealed call_me_maybe frame carrying the
// agent's candidate list and ships it through the peer's HomeRelay
// session. The peer's handleCallMeMaybe will (a) merge the candidates
// into its probe target set as cmmHints and (b) immediately fire one
// fire-and-forget probe per candidate so its NAT mapping toward us
// opens. When both sides do this in the same window, the EIM mapping
// converges and the next regular probe pair completes RTT-sampling on
// the direct path.
//
// peerNodePub is the std-base64 key used as the s.peers map key.
// SendCallMeMaybe looks up the peer to get the raw curve25519 bytes
// required for AEAD ECDH; an unknown peer is rejected.
//
// Returns an error if encoding or the relay send fails. Empty candidate
// lists are rejected — there is no useful frame to ship.
func (s *Service) SendCallMeMaybe(peerNodePub, peerDeviceID, peerNodeKey, relayURL string, candidates []netip.AddrPort) error {
	if len(candidates) == 0 {
		return errors.New("disco: SendCallMeMaybe with empty candidate list")
	}
	if len(candidates) > wireframe.MaxCandidateListLen {
		candidates = candidates[:wireframe.MaxCandidateListLen]
	}
	dstNodePub, ok := s.lookupNodePub(peerNodePub)
	if !ok {
		return errors.New("disco: SendCallMeMaybe peer not in NetworkMap")
	}
	out, err := s.encodeCallMeMaybeFrame(peerDeviceID, candidates, dstNodePub)
	if err != nil {
		return err
	}
	if err := s.cfg.Bind.SendDiscoViaRelay(out, peerDeviceID, peerNodeKey, relayURL); err != nil {
		return err
	}
	s.logger.Debug("disco: sent call_me_maybe",
		"device_id", peerDeviceID, "url", relayURL, "candidates", len(candidates))
	return nil
}

// lookupNodePub returns the peer's raw curve25519 public from the
// s.peers map keyed by std-base64 NodePub. Falls back to decoding the
// base64 key directly when the map lookup fails, so callers that hold
// the std-base64 string but the peer hasn't been ingested yet (rare;
// CMM tests construct minimal Services) still work. Returns ok=false
// when both paths fail.
func (s *Service) lookupNodePub(peerNodePub string) ([wireframe.NodeKeySize]byte, bool) {
	var zero [wireframe.NodeKeySize]byte
	s.mu.Lock()
	p, ok := s.peers[peerNodePub]
	s.mu.Unlock()
	if ok && p.nodePub != zero {
		return p.nodePub, true
	}
	raw, err := base64.StdEncoding.DecodeString(peerNodePub)
	if err != nil || len(raw) != wireframe.NodeKeySize {
		return zero, false
	}
	var out [wireframe.NodeKeySize]byte
	copy(out[:], raw)
	return out, true
}

// encodeCallMeMaybeFrame builds a sealed call_me_maybe frame with a
// fresh nonce. Mirrors encodeProbeFrame but populates CandidateList and
// has no CandidateID (CMM is a broadcast invite, not a per-candidate
// probe).
func (s *Service) encodeCallMeMaybeFrame(dstDeviceID string, candidates []netip.AddrPort, dstNodePub [wireframe.NodeKeySize]byte) ([]byte, error) {
	frame := &wireframe.Frame{
		Type:          wireframe.TypeCallMeMaybe,
		SrcDeviceID:   s.cfg.SelfDeviceID,
		DstDeviceID:   dstDeviceID,
		HasNonce:      true,
		Nonce:         freshNonce(),
		HasTimestamp:  true,
		Timestamp:     uint64(s.now().UnixMilli()),
		CandidateList: append([]netip.AddrPort(nil), candidates...),
	}
	return wireframe.EncodeSealed(frame, s.cfg.SelfNodeKeyPriv, s.cfg.SelfNodeKeyPub, dstNodePub)
}

// handleCallMeMaybe processes an authenticated (AEAD-opened) CMM frame,
// stores its candidate list as receiver-side cmmHints with a TTL, and
// fires one fire-and-forget direct probe per candidate so the local NAT
// mapping toward each addr opens immediately. The hint TTL ensures
// subsequent regular probe cycles also target the addrs (keeping the
// hole open across cycles), at which point any returning pong follows
// the normal pong-correlation path and produces RTT samples for the
// reconciler.
//
// Sender identity comes from the AEAD itself (srcNodeKey, cross-checked
// against the CP-published NodePub for the peer claiming SrcDeviceID).
// A replayed CMM frame is rejected via consumeNonce so it can't be
// used as a UDP-reflection amplifier.
func (s *Service) handleCallMeMaybe(f *wireframe.Frame, pkt wireframe.Inbound, srcNodeKey [wireframe.NodeKeySize]byte) {
	if f.SrcDeviceID == "" || !f.HasNonce {
		s.logger.Debug("call_me_maybe missing fields", "src", pkt.Src.String(), "path", pkt.Path)
		return
	}
	if len(f.CandidateList) == 0 {
		s.logger.Debug("call_me_maybe with empty candidate list", "device_id", f.SrcDeviceID)
		return
	}
	if len(f.CandidateList) > wireframe.MaxCandidateListLen {
		// Defense in depth — the inner-body decoder already enforces
		// this cap when AEAD opens, but a future codec change shouldn't
		// open a DoS shape.
		s.logger.Debug("call_me_maybe candidate list over cap",
			"device_id", f.SrcDeviceID, "count", len(f.CandidateList))
		return
	}
	s.mu.Lock()
	var peerNodePub string
	var peerNodePubBytes [wireframe.NodeKeySize]byte
	for k, p := range s.peers {
		if p.deviceID == f.SrcDeviceID {
			peerNodePub = k
			peerNodePubBytes = p.nodePub
			break
		}
	}
	s.mu.Unlock()
	if peerNodePub == "" {
		s.logger.Debug("call_me_maybe from unknown device", "device_id", f.SrcDeviceID)
		return
	}
	if peerNodePubBytes != srcNodeKey {
		s.logger.Debug("cmm srcNodeKey does not match CP NodePub", "device_id", f.SrcDeviceID)
		return
	}
	if !s.consumeNonce(f.Nonce, s.now()) {
		s.logger.Debug("call_me_maybe replay", "device_id", f.SrcDeviceID)
		return
	}

	now := s.now()
	expiresAt := now.Add(s.cfg.CMMHintTTL)

	// Build the hint set under lock and send a snapshot of the addrs
	// so the fire-and-forget probes happen outside the lock.
	dsts := make([]string, 0, len(f.CandidateList))
	for _, c := range f.CandidateList {
		if !c.IsValid() {
			continue
		}
		dsts = append(dsts, makeUDPDstFromAddrPort(c))
	}
	if len(dsts) == 0 {
		s.logger.Debug("call_me_maybe with no valid candidates", "device_id", f.SrcDeviceID)
		return
	}

	s.mu.Lock()
	p, ok := s.peers[peerNodePub]
	if ok {
		p.cmmHints = mergeCmmHints(p.cmmHints, dsts, expiresAt)
		s.peers[peerNodePub] = p
	}
	s.mu.Unlock()

	for _, dst := range dsts {
		if err := s.sendProbeDirectFireAndForget(dst, f.SrcDeviceID, peerNodePubBytes); err != nil {
			s.logger.Debug("disco probe send (cmm)", "device_id", f.SrcDeviceID, "addr", dst, "err", err)
		}
	}

	s.logger.Info("disco: handled call_me_maybe",
		"device_id", f.SrcDeviceID,
		"candidates", len(dsts),
		"hint_ttl_ms", s.cfg.CMMHintTTL.Milliseconds(),
	)

	s.emit(EventCallMeMaybeReceived{
		PeerNodePub:  peerNodePub,
		PeerDeviceID: f.SrcDeviceID,
		Candidates:   append([]netip.AddrPort(nil), f.CandidateList...),
		At:           now,
	})

	// Fan out to any OnCallMeMaybe subscribers (testharness scenario
	// dispatcher). Snapshot the slice under lock, then invoke without
	// holding s.mu so callees may safely re-enter Service methods.
	s.mu.Lock()
	cbs := make([]func(string, string), len(s.cmmCallbacks))
	copy(cbs, s.cmmCallbacks)
	s.mu.Unlock()
	for _, cb := range cbs {
		cb(peerNodePub, f.SrcDeviceID)
	}
}

// mergeCmmHints adds (or refreshes) hints for each addr with the given
// expiresAt. If a hint with the same addr already exists, its
// expiresAt is bumped to the later of the two values rather than
// silently shortened. Caller holds s.mu.
func mergeCmmHints(existing []cmmHint, addrs []string, expiresAt time.Time) []cmmHint {
	out := make([]cmmHint, 0, len(existing)+len(addrs))
	seen := make(map[string]int, len(existing)+len(addrs))
	for _, h := range existing {
		seen[h.addr] = len(out)
		out = append(out, h)
	}
	for _, a := range addrs {
		if idx, ok := seen[a]; ok {
			if expiresAt.After(out[idx].expiresAt) {
				out[idx].expiresAt = expiresAt
			}
			continue
		}
		seen[a] = len(out)
		out = append(out, cmmHint{addr: a, expiresAt: expiresAt})
	}
	return out
}

// liveCmmHints returns the addrs of un-expired hints for the peer and
// rewrites p.cmmHints in place to drop expired entries. Caller holds
// s.mu.
func liveCmmHints(p *peerState, now time.Time) []string {
	if len(p.cmmHints) == 0 {
		return nil
	}
	kept := p.cmmHints[:0]
	addrs := make([]string, 0, len(p.cmmHints))
	for _, h := range p.cmmHints {
		if !h.expiresAt.After(now) {
			continue
		}
		kept = append(kept, h)
		addrs = append(addrs, h.addr)
	}
	p.cmmHints = kept
	return addrs
}

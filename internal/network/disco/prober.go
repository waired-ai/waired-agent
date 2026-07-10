package disco

import (
	"context"
	"encoding/hex"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// runProbeLoop periodically iterates known peers and re-probes their
// candidates while no recent pong has arrived. The two-side initiator
// pattern (Q5: B): both peers do this independently the moment they
// see each other in their NetworkMap, and the first matching probe/pong
// pair establishes a direct UDP path.
//
// Cadence is ProbeReprobeActive (15s default). The prober does NOT
// distinguish active vs idle — once a peer becomes direct_verified the
// reconciler stops engaging direct_probing on that peer, but the
// prober still emits keep-alive probes so NAT mappings stay open. The
// keep-alive interval matches WG's PersistentKeepalive (25s) within a
// constant; refreshing more frequently is wasteful but harmless.
func (s *Service) runProbeLoop(ctx context.Context) {
	// Brief warm-up so UpdatePeers can be called first.
	select {
	case <-ctx.Done():
		return
	case <-time.After(500 * time.Millisecond):
	}
	for {
		s.probeAllPeers(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.ProbeReprobeActive):
		}
	}
}

// probeAllPeers iterates the current peer set and sends one probe per
// candidate per peer (direct UDP) plus, when the peer has a HomeRelay
// configured, one probe through that relay. Each probe carries a fresh
// nonce that's tracked in pendingProbes for pong correlation. The
// relay-path probe lets the reconciler measure agent→relay→peer RTT
// for its asymmetric direct/relay ratio (Tailscale-style path
// selection).
//
// Probes for a peer are sent only if the peer's last probe was longer
// ago than half the reprobe interval — this damps re-probes when
// UpdatePeers fires repeatedly (e.g., during NetworkMap churn).
func (s *Service) probeAllPeers(ctx context.Context) {
	now := s.now()
	s.mu.Lock()
	peers := make(map[string]peerState, len(s.peers))
	for k, p := range s.peers {
		peers[k] = p
	}
	s.mu.Unlock()

	// Lazy-prune expired CMM hints under a single lock acquisition,
	// then take a snapshot of the live hint set per peer so the
	// outer loop can iterate without re-locking.
	hintsByPeer := make(map[string][]string, len(peers))
	s.mu.Lock()
	for nodePub := range peers {
		p := s.peers[nodePub]
		hints := liveCmmHints(&p, now)
		s.peers[nodePub] = p
		if len(hints) > 0 {
			hintsByPeer[nodePub] = hints
		}
	}
	s.mu.Unlock()

	for nodePub, p := range peers {
		if ctx.Err() != nil {
			return
		}
		if !p.lastProbeAt.IsZero() && now.Sub(p.lastProbeAt) < s.cfg.ProbeReprobeActive/2 {
			continue
		}
		// One direct-path round per peer per probeAllPeers iteration.
		// Allocated even if sent==0 (cheap atomic increment); we just
		// skip the finalize call so the directRounds map stays empty.
		roundID := s.roundIDCounter.Add(1)
		var sent int
		var directSent uint16
		// Probe targets = published direct candidates ∪ live CMM hints.
		// CMM hints are de-duped against published candidates so we
		// don't probe the same addr twice in one cycle.
		seen := make(map[string]struct{}, len(p.candidates)+len(hintsByPeer[nodePub]))
		for _, addr := range p.candidates {
			if ctx.Err() != nil {
				return
			}
			if !isDirectUDPAddr(addr) {
				continue
			}
			if _, dup := seen[addr]; dup {
				continue
			}
			seen[addr] = struct{}{}
			if err := s.sendProbeDirect(addr, nodePub, p.deviceID, p.nodePub, roundID); err != nil {
				s.logger.Debug("disco probe send (direct)", "device_id", p.deviceID, "addr", addr, "err", err)
				continue
			}
			sent++
			directSent++
		}
		for _, addr := range hintsByPeer[nodePub] {
			if ctx.Err() != nil {
				return
			}
			if !isDirectUDPAddr(addr) {
				continue
			}
			if _, dup := seen[addr]; dup {
				continue
			}
			seen[addr] = struct{}{}
			if err := s.sendProbeDirect(addr, nodePub, p.deviceID, p.nodePub, roundID); err != nil {
				s.logger.Debug("disco probe send (cmm hint)", "device_id", p.deviceID, "addr", addr, "err", err)
				continue
			}
			sent++
			directSent++
		}
		if p.relayURL != "" {
			if err := s.sendProbeViaRelay(nodePub, p.deviceID, p.nodeKey, p.nodePub, p.relayURL); err != nil {
				s.logger.Debug("disco probe send (relay)", "device_id", p.deviceID, "url", p.relayURL, "err", err)
			} else {
				sent++
			}
		}
		// Mark the direct round's expected count so subsequent pong /
		// miss events can finalize it. If no direct probes were sent,
		// no round bookkeeping is needed (no events will reference
		// roundID). May synchronously emit if all candidates already
		// reported (extreme race; not expected in practice).
		if directSent > 0 {
			if ev, ok := s.finalizeRoundExpected(roundID, nodePub, p.deviceID, directSent, now); ok {
				s.emit(ev)
			}
		}
		if sent == 0 {
			continue
		}
		s.mu.Lock()
		p2 := s.peers[nodePub]
		p2.lastProbeAt = now
		s.peers[nodePub] = p2
		s.mu.Unlock()
	}
}

// finalizeRoundExpected stamps the expected probe count for a direct
// round and reports whether the round is already complete (all
// candidates have already reported a pong or miss before this call —
// possible only under extreme network speed). Returns (event, true)
// when the round finalizes synchronously here; otherwise (zero, false).
func (s *Service) finalizeRoundExpected(roundID uint64, peerNodePub, peerDeviceID string, expected uint16, now time.Time) (EventProbeRoundFinalized, bool) {
	s.mu.Lock()
	rs, ok := s.directRounds[roundID]
	if !ok {
		rs = &directRoundState{
			peerNodePub:  peerNodePub,
			peerDeviceID: peerDeviceID,
			startedAt:    now,
		}
		s.directRounds[roundID] = rs
	}
	rs.expected = expected
	rs.finalized = true
	if rs.succeeded+rs.missed >= rs.expected {
		anySuccess := rs.succeeded > 0
		delete(s.directRounds, roundID)
		s.mu.Unlock()
		return EventProbeRoundFinalized{
			PeerNodePub:  peerNodePub,
			PeerDeviceID: peerDeviceID,
			Path:         wireframe.PathDirect,
			RoundID:      roundID,
			AnySuccess:   anySuccess,
			At:           now,
		}, true
	}
	s.mu.Unlock()
	return EventProbeRoundFinalized{}, false
}

// recordRoundResultLocked increments succeeded or missed on the
// directRoundState for roundID, creating the entry lazily if events
// arrive before finalizeRoundExpected. Returns (event, true) once the
// round is finalized AND all candidates have reported. Must be called
// with s.mu held; caller emits the event after releasing the lock.
func (s *Service) recordRoundResultLocked(roundID uint64, isSuccess bool, peerNodePub, peerDeviceID string, now time.Time) (EventProbeRoundFinalized, bool) {
	if roundID == 0 {
		return EventProbeRoundFinalized{}, false
	}
	rs, ok := s.directRounds[roundID]
	if !ok {
		rs = &directRoundState{
			peerNodePub:  peerNodePub,
			peerDeviceID: peerDeviceID,
			startedAt:    now,
		}
		s.directRounds[roundID] = rs
	}
	if isSuccess {
		rs.succeeded++
	} else {
		rs.missed++
	}
	if rs.finalized && rs.succeeded+rs.missed >= rs.expected {
		anySuccess := rs.succeeded > 0
		delete(s.directRounds, roundID)
		return EventProbeRoundFinalized{
			PeerNodePub:  rs.peerNodePub,
			PeerDeviceID: rs.peerDeviceID,
			Path:         wireframe.PathDirect,
			RoundID:      roundID,
			AnySuccess:   anySuccess,
		}, true
	}
	return EventProbeRoundFinalized{}, false
}

// sendProbeDirect encodes a sealed disco_probe and ships it over direct
// UDP to dst. The probe's nonce is recorded in pendingProbes (path =
// "direct") so an incoming pong can be matched and attributed.
// roundID groups this probe with sibling candidates sent in the same
// probeAllPeers iteration so the reconciler sees one round-finalized
// event regardless of how many candidates were tried.
func (s *Service) sendProbeDirect(dst, peerNodePub, dstDeviceID string, dstNodePubBytes [wireframe.NodeKeySize]byte, roundID uint64) error {
	out, nonce, sentAt, err := s.encodeProbeFrame(dstDeviceID, dst, dstNodePubBytes)
	if err != nil {
		return err
	}
	s.recordPendingProbe(nonce, sentAt, wireframe.PathDirect, peerNodePub, dstDeviceID, roundID)
	if err := s.cfg.Bind.SendDisco(out, dst); err != nil {
		// On send failure, drop the pending entry — there will be no
		// pong to correlate, and we don't want to count this as a peer
		// miss when the local socket erred.
		s.deletePendingProbe(nonce)
		return err
	}
	return nil
}

// sendProbeViaRelay encodes a sealed disco_probe and ships it through
// the peer's HomeRelay session. The probe's nonce is recorded with
// path = "relay" so the reconciler can attribute the eventual pong RTT
// (or miss) to the relay path.
func (s *Service) sendProbeViaRelay(peerNodePub, dstDeviceID, dstNodeKey string, dstNodePubBytes [wireframe.NodeKeySize]byte, relayURL string) error {
	out, nonce, sentAt, err := s.encodeProbeFrame(dstDeviceID, "relay:"+relayURL, dstNodePubBytes)
	if err != nil {
		return err
	}
	// Relay-path probes carry roundID=0 (1 round = 1 probe, no
	// bookkeeping needed). handlePong / gc filter on roundID==0 to
	// skip round-finalize emission for relay.
	s.recordPendingProbe(nonce, sentAt, wireframe.PathRelay, peerNodePub, dstDeviceID, 0)
	if err := s.cfg.Bind.SendDiscoViaRelay(out, dstDeviceID, dstNodeKey, relayURL); err != nil {
		s.deletePendingProbe(nonce)
		return err
	}
	return nil
}

// sendProbeDirectFireAndForget ships a sealed disco_probe over direct
// UDP without recording a pendingProbe entry. Used by handleCallMeMaybe
// to immediately open the local NAT mapping toward each candidate the
// peer advertised. We don't expect a matching pong on this exact frame
// (the peer was the one that asked us to send, so it isn't necessarily
// listening for THIS nonce); the goal is to warm the conntrack so the
// next regular probe cycle finds an open hole. Any pong that does
// arrive matches the regular prober's pendingProbes the next cycle
// around — none of CMM's accounting is on the miss-streak path.
func (s *Service) sendProbeDirectFireAndForget(dst, dstDeviceID string, dstNodePubBytes [wireframe.NodeKeySize]byte) error {
	out, _, _, err := s.encodeProbeFrame(dstDeviceID, dst, dstNodePubBytes)
	if err != nil {
		return err
	}
	return s.cfg.Bind.SendDisco(out, dst)
}

// encodeProbeFrame builds a sealed probe with a fresh nonce. Returns
// (encoded bytes, nonce, sentAt) so the caller can record the pending
// state with the correct timestamp. dstNodePub is the receiver's
// curve25519 public — required by EncodeSealed for ECDH.
func (s *Service) encodeProbeFrame(dstDeviceID, candidateID string, dstNodePub [wireframe.NodeKeySize]byte) ([]byte, [wireframe.NonceSize]byte, time.Time, error) {
	nonce := freshNonce()
	now := s.now()
	probe := &wireframe.Frame{
		Type:         wireframe.TypeProbe,
		SrcDeviceID:  s.cfg.SelfDeviceID,
		DstDeviceID:  dstDeviceID,
		HasNonce:     true,
		Nonce:        nonce,
		HasTimestamp: true,
		Timestamp:    uint64(now.UnixMilli()),
		CandidateID:  candidateID,
	}
	out, err := wireframe.EncodeSealed(probe, s.cfg.SelfNodeKeyPriv, s.cfg.SelfNodeKeyPub, dstNodePub)
	if err != nil {
		return nil, nonce, now, err
	}
	return out, nonce, now, nil
}

func (s *Service) recordPendingProbe(nonce [wireframe.NonceSize]byte, sentAt time.Time, path, peerNodePub, peerDeviceID string, roundID uint64) {
	key := hex.EncodeToString(nonce[:])
	s.mu.Lock()
	s.pendingProbes[key] = pendingProbe{
		sentAt:       sentAt,
		path:         path,
		peerNodePub:  peerNodePub,
		peerDeviceID: peerDeviceID,
		roundID:      roundID,
	}
	s.mu.Unlock()
}

func (s *Service) deletePendingProbe(nonce [wireframe.NonceSize]byte) {
	key := hex.EncodeToString(nonce[:])
	s.mu.Lock()
	delete(s.pendingProbes, key)
	s.mu.Unlock()
}

// isDirectUDPAddr reports whether an endpoint string is a direct UDP
// candidate (vs relay or unknown). Only direct candidates are probed
// via SendDisco; relay candidates use SendDiscoViaRelay (gated on
// peerState.relayURL, not the candidate list).
func isDirectUDPAddr(s string) bool {
	if len(s) < 5 {
		return false
	}
	return s[:5] == "udp4:" || s[:5] == "udp6:"
}

package disco

import (
	"context"
	"encoding/hex"
	"net/netip"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// runInbound reads frames off the bind's DiscoInbound channel and
// dispatches by frame type:
//
//   - stun_response: plaintext + HMAC (frame.go path). Matches against
//     an outstanding observe waiter started by runObserveLoop.
//   - probe / pong / call_me_maybe: AEAD-sealed (sealed.go path). The
//     AEAD open both proves the sender owns the matching NodeKey priv
//     AND yields the inner frame in one step — no separate signature
//     check is needed. We still cross-check srcNodeKey from the wire
//     against the CP-published NodePub for the SrcDeviceID so a stolen
//     NodeKey can't be grafted onto a different DeviceID.
func (s *Service) runInbound(ctx context.Context) {
	in := s.cfg.Bind.DiscoInbound()
	for {
		select {
		case <-ctx.Done():
			return
		case pkt, ok := <-in:
			if !ok {
				return
			}
			s.handleInbound(pkt)
		}
	}
}

func (s *Service) handleInbound(pkt wireframe.Inbound) {
	if wireframe.IsSealed(pkt.Payload) {
		s.handleSealedInbound(pkt)
		return
	}
	// Plaintext path = STUN frames only (peer↔peer moved to sealed).
	f, err := wireframe.Decode(pkt.Payload)
	if err != nil {
		s.logger.Debug("disco inbound decode", "err", err, "src", pkt.Src.String())
		return
	}
	if f.HasTimestamp {
		now := s.now()
		ts := time.UnixMilli(int64(f.Timestamp))
		if !timestampInWindow(now, ts, s.cfg.ProbeWindow) {
			s.logger.Debug("disco inbound out of window",
				"src", pkt.Src.String(), "drift", now.Sub(ts).String())
			return
		}
	}
	switch f.Type {
	case wireframe.TypeSTUNResponse:
		s.handleSTUNResponse(f, pkt)
	case wireframe.TypeProbe, wireframe.TypePong, wireframe.TypeCallMeMaybe:
		// Peer↔peer frames MUST be AEAD-sealed on v1; a plaintext one
		// is either a misconfigured peer (rolled back to pre-AEAD build)
		// or an attempted downgrade. Drop silently with a debug log so
		// the operational signal lives in disco event counters, not in
		// noisy warn-level chatter.
		s.logger.Debug("disco: dropped plaintext peer frame (sealed required)",
			"type", f.Type.String(), "src", pkt.Src.String())
	default:
		s.logger.Debug("disco inbound unhandled type",
			"type", f.Type.String(), "src", pkt.Src.String())
	}
}

// handleSealedInbound opens an AEAD-protected disco frame and dispatches
// it to the per-type handler. srcNodeKey returned by DecodeSealed is the
// authenticated sender identity — handlers cross-check it against the
// CP-published NodePub of the peer claiming SrcDeviceID.
func (s *Service) handleSealedInbound(pkt wireframe.Inbound) {
	f, srcNodeKey, err := wireframe.DecodeSealed(pkt.Payload, s.cfg.SelfNodeKeyPriv, s.cfg.SelfNodeKeyPub)
	if err != nil {
		s.logger.Debug("disco sealed decode", "err", err, "src", pkt.Src.String(), "path", pkt.Path)
		return
	}
	if f.HasTimestamp {
		now := s.now()
		ts := time.UnixMilli(int64(f.Timestamp))
		if !timestampInWindow(now, ts, s.cfg.ProbeWindow) {
			s.logger.Debug("disco sealed out of window",
				"src", pkt.Src.String(), "drift", now.Sub(ts).String())
			return
		}
	}
	switch f.Type {
	case wireframe.TypeProbe:
		s.handleProbe(f, pkt, srcNodeKey)
	case wireframe.TypePong:
		s.handlePong(f, pkt, srcNodeKey)
	case wireframe.TypeCallMeMaybe:
		s.handleCallMeMaybe(f, pkt, srcNodeKey)
	case wireframe.TypeSTUNRequest, wireframe.TypeSTUNResponse:
		// STUN frames keep the plaintext+HMAC path in this release.
		// A sealed STUN frame is not yet supported (would need a
		// relay-published X25519 pub for ECDH). Drop silently with
		// debug visibility.
		s.logger.Debug("disco: sealed STUN frame not supported in v1",
			"type", f.Type.String(), "src", pkt.Src.String())
	default:
		s.logger.Debug("disco sealed inbound unhandled type",
			"type", f.Type.String(), "src", pkt.Src.String())
	}
}

func (s *Service) handleSTUNResponse(f *wireframe.Frame, pkt wireframe.Inbound) {
	if !f.HasNonce || !f.HasObserved {
		s.logger.Debug("stun_response missing nonce or observed", "src", pkt.Src.String())
		return
	}
	if len(s.cfg.RelaySharedSecret) > 0 {
		if err := verifyFrameHMAC(pkt.Payload, s.cfg.RelaySharedSecret, f.HMACTag); err != nil {
			s.logger.Debug("stun_response hmac verify", "err", err, "src", pkt.Src.String())
			return
		}
	}
	nonceHex := hex.EncodeToString(f.Nonce[:])
	s.mu.Lock()
	w, ok := s.pendingObserve[nonceHex]
	if ok {
		delete(s.pendingObserve, nonceHex)
	}
	s.mu.Unlock()
	if !ok {
		// Stale or unknown nonce — possibly response after timeout.
		return
	}
	// Telemetry: per-family response counter, paired with
	// stunAttempts{V4,V6} on the send side (observer.go). The verifier
	// compares attempts vs responses to localise the v6 flake; an
	// attempts ≫ responses gap on v6 (with v4 in balance) indicates
	// agent → relay BGP path works but relay → agent return path
	// (or its v6 inbound socket) is lossy.
	if isV6Source(pkt.Src.Addr()) {
		s.stunResponsesV6.Add(1)
	} else {
		s.stunResponsesV4.Add(1)
	}
	select {
	case w.resultCh <- f.ObservedAddr:
	default:
	}
}

// isV6Source returns true for a true IPv6 source address (not the
// v4-in-v6 mapped form ::ffff:a.b.c.d). The agent's StdNetBind may
// receive v4 packets on its v6 socket as 4-in-6; those should still
// count as v4 for the per-family STUN telemetry.
func isV6Source(a netip.Addr) bool {
	return a.Is6() && !a.Is4In6()
}

func (s *Service) handleProbe(f *wireframe.Frame, pkt wireframe.Inbound, srcNodeKey [wireframe.NodeKeySize]byte) {
	if !f.HasNonce || f.SrcDeviceID == "" {
		s.logger.Debug("probe missing fields", "src", pkt.Src.String(), "path", pkt.Path)
		return
	}
	// Find the peer by device_id and cross-check srcNodeKey. The AEAD
	// already proved the sender owns the matching NodeKey priv; this
	// check makes sure the CP knows about that pair (otherwise a
	// rolled-key peer or a NodeKey grafted onto a different DeviceID is
	// silently dropped).
	s.mu.Lock()
	var peerNodeKey string
	var peerNodePub [wireframe.NodeKeySize]byte
	var found bool
	peerLog := f.SrcDeviceID
	for _, p := range s.peers {
		if p.deviceID == f.SrcDeviceID {
			peerNodeKey = p.nodeKey
			peerNodePub = p.nodePub
			if p.logName != "" {
				peerLog = p.logName
			}
			found = true
			break
		}
	}
	s.mu.Unlock()
	if !found {
		s.logger.Debug("probe from unknown device", "device_id", f.SrcDeviceID)
		return
	}
	if peerNodePub != srcNodeKey {
		s.logger.Debug("probe srcNodeKey does not match CP NodePub", "device_id", peerLog)
		return
	}
	if !s.consumeNonce(f.Nonce, s.now()) {
		s.logger.Debug("probe replay", "device_id", peerLog)
		return
	}

	pong := &wireframe.Frame{
		Type:         wireframe.TypePong,
		SrcDeviceID:  s.cfg.SelfDeviceID,
		DstDeviceID:  f.SrcDeviceID,
		HasNonce:     true,
		Nonce:        f.Nonce, // echo so prober can correlate
		HasTimestamp: true,
		Timestamp:    uint64(s.now().UnixMilli()),
	}
	// Direct-UDP-arrived probes: include observed_outer so the prober
	// can learn its public NAT-mapped addr. Relay-tunnelled probes
	// don't carry a meaningful direct src (the relay session is the
	// "src"), so we omit observed_outer — the prober's own STUN
	// observer will fill that gap.
	if pkt.Path != wireframe.PathRelay {
		srcCanonical := pkt.Src
		if a := pkt.Src.Addr(); a.Is4In6() {
			srcCanonical = netip.AddrPortFrom(a.Unmap(), pkt.Src.Port())
		}
		pong.HasObserved = true
		pong.ObservedAddr = srcCanonical
	}

	out, err := wireframe.EncodeSealed(pong, s.cfg.SelfNodeKeyPriv, s.cfg.SelfNodeKeyPub, peerNodePub)
	if err != nil {
		s.logger.Warn("disco: encode pong", "err", err)
		return
	}

	if pkt.Path == wireframe.PathRelay {
		if err := s.cfg.Bind.SendDiscoViaRelay(out, f.SrcDeviceID, peerNodeKey, pkt.RelayURL); err != nil {
			s.logger.Warn("disco: send pong via relay", "device_id", peerLog, "url", pkt.RelayURL, "err", err)
		}
		return
	}
	dst := makeUDPDstFromAddrPort(pkt.Src)
	if err := s.cfg.Bind.SendDisco(out, dst); err != nil {
		s.logger.Warn("disco: send pong", "dst", dst, "err", err)
	}
}

func (s *Service) handlePong(f *wireframe.Frame, pkt wireframe.Inbound, srcNodeKey [wireframe.NodeKeySize]byte) {
	if f.SrcDeviceID == "" {
		s.logger.Debug("pong missing src", "src", pkt.Src.String(), "path", pkt.Path)
		return
	}
	s.mu.Lock()
	var peerNodePub string
	var peerNodePubBytes [wireframe.NodeKeySize]byte
	peerLog := f.SrcDeviceID
	for k, p := range s.peers {
		if p.deviceID == f.SrcDeviceID {
			peerNodePub = k
			peerNodePubBytes = p.nodePub
			if p.logName != "" {
				peerLog = p.logName
			}
			break
		}
	}
	s.mu.Unlock()
	if peerNodePub == "" {
		s.logger.Debug("pong from unknown device", "device_id", f.SrcDeviceID)
		return
	}
	if peerNodePubBytes != srcNodeKey {
		s.logger.Debug("pong srcNodeKey does not match CP NodePub", "device_id", peerLog)
		return
	}
	// Was this pong solicited by a probe we sent?
	nonceHex := hex.EncodeToString(f.Nonce[:])
	s.mu.Lock()
	pp, expected := s.pendingProbes[nonceHex]
	if expected {
		delete(s.pendingProbes, nonceHex)
	}
	now := s.now()
	rtt := now.Sub(pp.sentAt)
	// Fold the sample into the per-peer EMA used by Phase 7 routing
	// and stamp lastPongAt for Phase 8's ReachableSnapshot. Both run
	// inside the existing mu region so the map mutations piggyback on
	// the lock the pendingProbes delete already needed.
	// Skips when !expected: rtt is meaningless without a sentAt, and
	// an unsolicited pong (replay / stale) is not a proof of current
	// two-way reachability either.
	var roundEvent EventProbeRoundFinalized
	var roundDone bool
	if expected {
		s.recordRTTSample(f.SrcDeviceID, rtt)
		s.recordPongReceived(f.SrcDeviceID)
		if pp.path == wireframe.PathDirect && pp.roundID != 0 {
			roundEvent, roundDone = s.recordRoundResultLocked(pp.roundID, true, pp.peerNodePub, pp.peerDeviceID, now)
		}
	}
	s.mu.Unlock()
	if roundDone {
		s.emit(roundEvent)
	}
	if !expected {
		s.logger.Debug("pong with unknown nonce; replay or stale", "device_id", peerLog)
		return
	}
	// Path consistency check: the sender's per-nonce path tag should
	// agree with the transport the pong came in on. Mismatch is rare
	// (possible if the receiver pongs back over a different path than
	// the probe arrived on; current handleProbe enforces same-path
	// reply). Trust the sender's tag (pp.path) for RTT attribution
	// since that's where the timestamp was captured.
	s.emit(EventProbeRTTSampled{
		PeerNodePub:  peerNodePub,
		PeerDeviceID: f.SrcDeviceID,
		Path:         pp.path,
		RTT:          rtt,
		At:           now,
	})
	// EventPongFromPeer remains the signal the reconciler uses to
	// adopt observed_addr for the direct path. Relay-tunnelled pongs
	// don't carry useful direct src info, so skip the event in that
	// case (the reconciler doesn't need a "peer reachable via relay"
	// hint — it already has the relay path available unconditionally).
	if pp.path == wireframe.PathDirect {
		ev := EventPongFromPeer{
			PeerNodePub:  peerNodePub,
			PeerDeviceID: f.SrcDeviceID,
			DirectSrc:    pkt.Src,
			ReceivedAt:   now,
		}
		if f.HasObserved {
			ev.ObservedSelfAddr = f.ObservedAddr
		}
		s.emit(ev)
		s.logger.Info("disco: peer reachable on direct UDP",
			"device_id", peerLog,
			"direct_src", pkt.Src.String(),
			"observed_self", ev.ObservedSelfAddr.String(),
			"rtt_ms", rtt.Milliseconds(),
		)
		return
	}
	s.logger.Debug("disco: peer pong via relay",
		"device_id", peerLog,
		"relay_url", pkt.RelayURL,
		"rtt_ms", rtt.Milliseconds(),
	)
}

// consumeNonce returns true on first sight of nonce within ProbeWindow.
// Used to prevent probe replay from a stale capture.
func (s *Service) consumeNonce(nonce [wireframe.NonceSize]byte, now time.Time) bool {
	key := hex.EncodeToString(nonce[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if seen, ok := s.nonceCache[key]; ok && now.Sub(seen) <= s.cfg.ProbeWindow {
		return false
	}
	s.nonceCache[key] = now
	return true
}

// makeUDPDstFromAddrPort formats an AddrPort as wgnet's
// "udp4:host:port" or "udp6:[host]:port" syntax. (separate from
// makeUDPDst for callers that have a netip.AddrPort, not host+port.)
func makeUDPDstFromAddrPort(ap netip.AddrPort) string {
	if ap.Addr().Is4() {
		return "udp4:" + ap.String()
	}
	return "udp6:" + ap.String()
}

// runGCLoop periodically prunes nonceCache and pendingProbes to keep
// memory bounded under steady traffic, and emits EventProbeMissed for
// probes that aged out without a matching pong. The cadence is tied to
// ProbeReprobeActive so a missed probe surfaces within roughly
// 1×–2×ProbeReprobeActive — fast enough to drive the reconciler's
// asymmetric direct/relay path-selection without spamming the
// scheduler.
func (s *Service) runGCLoop(ctx context.Context) {
	period := s.cfg.ProbeReprobeActive
	if period <= 0 {
		period = time.Minute
	}
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.gc()
		}
	}
}

func (s *Service) gc() {
	now := s.now()
	s.mu.Lock()
	for k, t := range s.nonceCache {
		if now.Sub(t) > s.cfg.ProbeWindow*2 {
			delete(s.nonceCache, k)
		}
	}
	// Drain timed-out probes into a local slice so we can emit
	// EventProbeMissed without holding s.mu (emit() does a non-blocking
	// channel send, but it doesn't take the lock and we want to avoid
	// re-entrant lock surprises if a future hook does).
	type missedProbe struct {
		peerNodePub  string
		peerDeviceID string
		path         string
		roundID      uint64
	}
	var missed []missedProbe
	for k, p := range s.pendingProbes {
		if now.Sub(p.sentAt) > s.cfg.ProbeReprobeActive*2 {
			missed = append(missed, missedProbe{
				peerNodePub:  p.peerNodePub,
				peerDeviceID: p.peerDeviceID,
				path:         p.path,
				roundID:      p.roundID,
			})
			delete(s.pendingProbes, k)
		}
	}
	// Aggregate miss outcomes into directRounds under the same lock so
	// the round bookkeeping stays consistent with pendingProbes. A
	// round finalizes once succeeded+missed >= expected AND finalized
	// is true (set by finalizeRoundExpected after all sends).
	var roundEvents []EventProbeRoundFinalized
	for _, m := range missed {
		if m.path != wireframe.PathDirect || m.roundID == 0 {
			continue
		}
		if ev, done := s.recordRoundResultLocked(m.roundID, false, m.peerNodePub, m.peerDeviceID, now); done {
			roundEvents = append(roundEvents, ev)
		}
	}
	// Theoretical leak guard: drop directRounds entries that have been
	// open beyond 2× ProbeReprobeActive without finalizing. In practice
	// finalizeRoundExpected is called synchronously from probeAllPeers
	// so this only fires if a probe send failed silently mid-round
	// (and the lost-send wasn't accounted in expected). Without this
	// the map would grow unboundedly on persistent send errors.
	leakHorizon := s.cfg.ProbeReprobeActive * 2
	for id, rs := range s.directRounds {
		if now.Sub(rs.startedAt) > leakHorizon {
			delete(s.directRounds, id)
		}
	}
	s.mu.Unlock()
	for _, m := range missed {
		s.emit(EventProbeMissed{
			PeerNodePub:  m.peerNodePub,
			PeerDeviceID: m.peerDeviceID,
			Path:         m.path,
			At:           now,
		})
	}
	for _, ev := range roundEvents {
		s.emit(ev)
	}
}

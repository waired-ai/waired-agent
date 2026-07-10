package disco

import (
	"context"
	"encoding/hex"
	"net/netip"
	"strings"
	"time"

	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// runObserveLoop polls every known relay's UDP STUN-echo on each disco
// port and feeds the response addr through emit / NAT-type detection.
//
// Cadence (per cfg.STUNObserveLearning / Active / Idle):
//   - while firstObservedV6At is still zero (= the agent has never
//     observed v6 yet, "v6-learning" phase) → STUNObserveLearning
//     (10s default). Shortens the v6 first-observation tail on testnet,
//     where v4 typically converges within seconds of relay listen but
//     v6 trails by 10+ sweeps × 60s under the active cadence (relay
//     v6 NIC init + per-VM /96 route propagation are both slower than
//     v4).
//   - once v6 has landed → STUNObserveActive (60s default).
//   - the eventual idle interval (STUNObserveIdle, 5min default) is
//     applied by the reconciler in step 7, not by this loop.
//
// Earlier revisions used s.observed.IsValid() as the learning gate.
// That gate was too broad: v4 converges within seconds of relay
// listen, so observed.IsValid() flips true while v6 is still missing,
// and the loop falls back to active (60s) cadence — exactly the wait
// we want to shorten. Empirically: agent a1-native in run 26014459529
// observed v4 at +30s after relay listen, then waited 12 sweeps × 60s
// = 12 min for v6. Keying on firstObservedV6At specifically keeps the
// loop at 10s cadence through that window; v4-only environments
// (no v6 NIC) stay at 10s indefinitely — relay load remains small,
// and we'd rather pay that than miss v6 under flaky conditions.
func (s *Service) runObserveLoop(ctx context.Context) {
	// Brief warm-up: wait one tick so callers have a chance to call
	// UpdateRelays before we observe.
	select {
	case <-ctx.Done():
		return
	case <-time.After(500 * time.Millisecond):
	}
	for {
		s.observeOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.relayUpdate:
			// Eager first-probe: UpdateRelays learned a new/changed relay
			// set, so sweep now instead of sleeping out the interval. On
			// testnet this removes the up-to-one-interval dead wait between
			// receiving the relay in the NetworkMap and the first STUN
			// probe (v4 + v6). Same probes, same discovery — pure latency.
		case <-time.After(s.nextObserveInterval()):
		}
	}
}

// nextObserveInterval returns the cadence the loop should wait before
// the next sweep. STUNObserveLearning is used while v6 has never been
// observed (firstObservedV6At == zero); STUNObserveActive is used once
// v6 lands. Reading firstObservedV6At under s.mu keeps the switch
// consistent with observeOnce's stamp.
func (s *Service) nextObserveInterval() time.Duration {
	s.mu.Lock()
	v6Observed := !s.firstObservedV6At.IsZero()
	s.mu.Unlock()
	if !v6Observed {
		return s.cfg.STUNObserveLearning
	}
	return s.cfg.STUNObserveActive
}

type observeSample struct {
	relayHost string
	port      int
	addr      netip.AddrPort
}

// observeOnce probes every (relay, port) pair, collects observed addrs,
// updates self.observed and self.natType, and emits events on change.
func (s *Service) observeOnce(ctx context.Context) {
	s.mu.Lock()
	urls := append([]string(nil), s.relayURLs...)
	prevObs := s.observed
	prevNAT := s.natType
	s.mu.Unlock()

	if len(urls) == 0 {
		return
	}

	// Collect (port, observed_addr) tuples. We keep them grouped by
	// relay so symmetric-NAT detection compares same-relay-different-port
	// observations rather than across relays (cleaner signal).
	var samples []observeSample
	for _, u := range urls {
		host, err := extractHostFromRelayURL(u)
		if err != nil {
			s.logger.Debug("disco observer: parse relay url", "url", u, "err", err)
			continue
		}
		for _, port := range s.cfg.DiscoUDPPorts {
			obs, ok := s.observeOne(ctx, host, port)
			if !ok {
				continue
			}
			samples = append(samples, observeSample{relayHost: host, port: port, addr: obs})
		}
	}

	if len(samples) == 0 {
		s.logger.Info("disco observer: 0 samples", "url_count", len(urls), "port_count", len(s.cfg.DiscoUDPPorts))
		return
	}
	familyCount := struct{ v4, v6 int }{}
	for _, smp := range samples {
		if smp.addr.Addr().Is6() {
			familyCount.v6++
		} else {
			familyCount.v4++
		}
	}
	s.logger.Info("disco observer: samples", "v4", familyCount.v4, "v6", familyCount.v6, "url_count", len(urls))

	// NAT-type heuristic: pick a relay where we have ≥2 samples. If the
	// observed src ports are equal → EIM (endpoint-independent); else →
	// symmetric. With <2 samples on any single relay we can't decide
	// and leave NATTypeUnknown.
	natType := NATTypeUnknown
	bestObs := samples[0].addr
	hostSeen := map[string]bool{}
	for _, smp := range samples {
		if hostSeen[smp.relayHost] {
			continue
		}
		hostSeen[smp.relayHost] = true
		var ports []netip.AddrPort
		for _, s2 := range samples {
			if s2.relayHost == smp.relayHost {
				ports = append(ports, s2.addr)
			}
		}
		if len(ports) < 2 {
			continue
		}
		if ports[0].Port() == ports[1].Port() {
			natType = NATTypeEIM
		} else {
			natType = NATTypeSymmetric
		}
		// Prefer a v6 sample when available — GUA is end-to-end, so the
		// observed addr is the agent's actual public v6 (no NAT). This
		// also lets the testnet's ipv6 verifier (which asserts
		// observed_addr is a bracketed v6 literal) flip cleanly when
		// the relay advertises both families via DiscoHosts. Race
		// between v4 / v6 sample arrival would otherwise yield a
		// flaky observed_addr family.
		if ports[0].Addr().Is6() {
			bestObs = ports[0]
			break
		}
		// First v4 with ≥2 samples is the fallback when no v6 ever
		// arrives. Keep iterating in case a later relay yields v6.
		bestObs = ports[0]
	}

	// Find the most recent v6 sample (any one — the observer doesn't
	// distinguish older vs newer within a round). Even if bestObs
	// landed on v4 this round, surface the v6 sample via
	// lastObservedV6 so verifiers / stats can confirm reachability.
	var v6Sample netip.AddrPort
	for _, smp := range samples {
		if smp.addr.Addr().Is6() {
			v6Sample = smp.addr
			break
		}
	}

	now := s.now()
	s.mu.Lock()
	s.observed = bestObs
	s.natType = natType
	if v6Sample.IsValid() {
		s.lastObservedV6 = v6Sample
		// Stamp the agent's first v6 observation, once. Used by
		// Status.FirstObservedV6Unix → Cloud Logging → testnet
		// verifier to build a per-agent v6-convergence-time
		// histogram across runs. firstObservedV6At is never reset
		// after the first stamp; lastObservedV6 already carries
		// the most-recent sample for liveness.
		if s.firstObservedV6At.IsZero() {
			s.firstObservedV6At = now
		}
	}
	s.mu.Unlock()

	if bestObs != prevObs {
		s.emit(EventObservedAddr{Addr: bestObs, NATType: natType, ObservedAt: now})
	}
	if natType != prevNAT {
		s.emit(EventNATTypeDetected{Kind: natType})
	}
}

// observeOne sends one stun_request and waits for the matching response
// up to STUNTimeout. Returns the observed AddrPort and ok=true on
// success, zero/false otherwise.
func (s *Service) observeOne(ctx context.Context, host string, port int) (netip.AddrPort, bool) {
	nonce := freshNonce()
	now := s.now()
	req := &wireframe.Frame{
		Type:         wireframe.TypeSTUNRequest,
		SrcDeviceID:  s.cfg.SelfDeviceID,
		HasNonce:     true,
		Nonce:        nonce,
		HasTimestamp: true,
		Timestamp:    uint64(now.UnixMilli()),
	}
	var out []byte
	var err error
	if len(s.cfg.RelaySharedSecret) > 0 {
		out, err = signFrameHMAC(req, s.cfg.RelaySharedSecret)
	} else {
		out, err = req.Encode()
	}
	if err != nil {
		s.logger.Warn("disco observer: encode stun_request", "err", err)
		return netip.AddrPort{}, false
	}

	waiter := observeWaiter{
		port:     port,
		deadline: now.Add(s.cfg.STUNTimeout),
		resultCh: make(chan netip.AddrPort, 1),
	}
	nonceHex := hex.EncodeToString(nonce[:])
	s.mu.Lock()
	s.pendingObserve[nonceHex] = waiter
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pendingObserve, nonceHex)
		s.mu.Unlock()
	}()

	dst := makeUDPDst(host, port)
	// Telemetry: per-family attempt counter. Mirror makeUDPDst's
	// "host contains ':' ⇒ v6" heuristic so the family classification
	// stays consistent with the actual wire-bound dst string. Counters
	// are read by STUNCounters() and surfaced via Status to Cloud
	// Logging, where the verifier compares attempts vs responses to
	// localise the v6 flake (no send vs no reply vs no observation).
	if strings.Contains(host, ":") {
		s.stunAttemptsV6.Add(1)
	} else {
		s.stunAttemptsV4.Add(1)
	}
	if err := s.cfg.Bind.SendDisco(out, dst); err != nil {
		s.logger.Warn("disco observer: send stun_request", "dst", dst, "err", err)
		return netip.AddrPort{}, false
	}

	select {
	case <-ctx.Done():
		return netip.AddrPort{}, false
	case <-time.After(s.cfg.STUNTimeout):
		s.logger.Info("disco observer: stun timeout", "dst", dst)
		return netip.AddrPort{}, false
	case obs := <-waiter.resultCh:
		return obs, true
	}
}

package disco

import (
	wireframe "github.com/waired-ai/waired-agent/proto/disco"
)

// inboundSource renders where an inbound disco frame came from, for the
// `src` log attribute.
//
// The two arrival paths carry different identification and only one of
// them has an address. Direct UDP frames arrive with the sender's
// ip:port. Relay-tunnelled frames do not: the relay session is the
// "src", and wgnet's fanInRelay leaves Inbound.Src at its zero value,
// whose String() is the literal "invalid AddrPort" — the string
// waired-agent#712 found in 737 consecutive decode failures with nothing
// to attribute them to. What the relay envelope does carry is the
// sender's device id, which is enough to name the peer.
//
// peerName is the caller's resolved display name for that device id
// (the Public Share grant pseudonym where there is one, per spec §8.5);
// empty when the device is not in the current peer set, in which case
// the raw id from the envelope is printed. An id we have never been
// told about cannot be a Public Share peer we hold a pseudonym for, and
// naming it is the whole point of the line — the same trade the
// existing "probe from unknown device" log already makes.
func inboundSource(pkt wireframe.Inbound, peerName string) string {
	if pkt.Path == wireframe.PathRelay {
		switch {
		case peerName != "":
			return peerName + " via relay"
		case pkt.RelaySrcDeviceID != "":
			return pkt.RelaySrcDeviceID + " via relay"
		default:
			return "unidentified sender via relay"
		}
	}
	if pkt.Src.IsValid() {
		return pkt.Src.String()
	}
	return "unknown source"
}

// srcOf is inboundSource with the peer-name lookup already done against
// the live peer set. Used at the sites that have no decoded frame to
// take a device id from — a frame that failed to decode, or one whose
// required fields are missing.
func (s *Service) srcOf(pkt wireframe.Inbound) string {
	return inboundSource(pkt, s.logNameForDevice(pkt.RelaySrcDeviceID))
}

// logNameForDevice resolves a device id to the identifier to print in
// logs, or "" when the device is not in the current peer set. Mirrors
// the peerLog lookup the frame handlers already do, hoisted so the
// pre-decode log sites can use it too.
func (s *Service) logNameForDevice(deviceID string) string {
	if deviceID == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.peers {
		if p.deviceID == deviceID {
			return p.logName
		}
	}
	return ""
}

package main

import (
	"fmt"
	"net"
	"strings"
)

// resolveLoginEndpoint expands a port-0 login endpoint into one bound to a
// concrete free UDP port, mirroring the CLI's local-enrollment path
// (chooseListenAddr -> newReservedUDPPort in cmd/waired).
//
// The daemon-driven login path advertises "udp4:" + --login-listen, whose
// default host:port is "127.0.0.1:0". Port 0 is a placeholder meaning "let the
// OS pick a port"; it must be resolved to a real port BEFORE enrollment because
// the endpoint is persisted verbatim into identity.json and later:
//   - parsed by udpListenPortFromEndpoint, which rejects port 0
//     ("port out of range: 0") and so fails activation, and
//   - fed to the WireGuard engine as its ListenPort.
//
// Resolving here (called immediately before enroll) keeps the reserve->bind
// window to a few seconds even on a daemon that has sat idle-unenrolled for a
// long time. A concrete (non-zero) port is returned unchanged. The optional
// "udp4:"/"udp6:" scheme prefix is preserved.
func resolveLoginEndpoint(endpoint string) (string, error) {
	scheme, rest := "", endpoint
	switch {
	case strings.HasPrefix(rest, "udp4:"):
		scheme, rest = "udp4:", strings.TrimPrefix(rest, "udp4:")
	case strings.HasPrefix(rest, "udp6:"):
		scheme, rest = "udp6:", strings.TrimPrefix(rest, "udp6:")
	}
	host, port, err := splitHostPort(rest)
	if err != nil {
		return "", err
	}
	if port != "0" {
		return endpoint, nil
	}
	p, err := reserveUDPPort(host)
	if err != nil {
		return "", fmt.Errorf("reserve udp port on %q: %w", host, err)
	}
	return fmt.Sprintf("%s%s:%d", scheme, host, p), nil
}

// reserveUDPPort binds host:0 to discover a free UDP port, then closes the
// socket and returns the port number. There is a small race between close and
// the WireGuard engine re-binding the port at activation, identical to the
// CLI's newReservedUDPPort (cmd/waired/helpers.go); production deployments that
// care pin a fixed port via --login-listen.
func reserveUDPPort(host string) (int, error) {
	addr, err := net.ResolveUDPAddr("udp", host+":0")
	if err != nil {
		return 0, err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return 0, err
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	if cerr := conn.Close(); cerr != nil {
		return 0, cerr
	}
	return port, nil
}

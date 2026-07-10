package main

import (
	"fmt"
	"net"
	"strings"
)

// splitHostPort returns ("host", "port", err) for "host:port" inputs.
// Permissive about IPv6 brackets - falls back to net.SplitHostPort when
// a `[` is present.
func splitHostPort(s string) (string, string, error) {
	if strings.Contains(s, "[") {
		return net.SplitHostPort(s)
	}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return "", "", fmt.Errorf("missing port in %q", s)
	}
	return s[:i], s[i+1:], nil
}

// newReservedUDPPort binds to host:0 to discover a free UDP port, then
// closes the socket and returns the port number. There is a small race
// between close and the agent re-binding, but for local-dev use that's
// fine; production deployments use --listen with a fixed port.
func newReservedUDPPort(host string) (int, error) {
	addr, err := net.ResolveUDPAddr("udp", host+":0")
	if err != nil {
		return 0, err
	}
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return 0, err
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	conn.Close()
	return port, nil
}

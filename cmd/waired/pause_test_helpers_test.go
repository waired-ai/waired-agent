package main

import (
	"io"
	"net"
	"testing"
)

// newClosedTCPAddr returns a "host:port" string for a TCP port that is
// guaranteed unused — we bind, capture the address, close the listener,
// and return. Tests use this to make a dial reliably fail with
// connection-refused instead of guessing at a free port.
func newClosedTCPAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr, nil
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("readAll: %v", err)
	}
	return body
}

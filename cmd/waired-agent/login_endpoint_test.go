package main

import (
	"strings"
	"testing"
)

func TestResolveLoginEndpoint_ResolvesPortZero(t *testing.T) {
	got, err := resolveLoginEndpoint("udp4:127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolveLoginEndpoint: %v", err)
	}
	if !strings.HasPrefix(got, "udp4:127.0.0.1:") {
		t.Fatalf("scheme/host not preserved: %q", got)
	}
	// The resolved endpoint must parse the same way activate() parses it via
	// udpListenPortFromEndpoint, which rejects port 0. This ties the resolver
	// directly to the failure it fixes (issue #576).
	port, err := udpListenPortFromEndpoint(got)
	if err != nil {
		t.Fatalf("resolved endpoint %q not concrete: %v", got, err)
	}
	if port < 1 || port > 65535 {
		t.Fatalf("resolved port out of range: %d", port)
	}
}

func TestResolveLoginEndpoint_PreservesUDP6Scheme(t *testing.T) {
	got, err := resolveLoginEndpoint("udp6:127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolveLoginEndpoint: %v", err)
	}
	if !strings.HasPrefix(got, "udp6:127.0.0.1:") {
		t.Fatalf("udp6 scheme not preserved: %q", got)
	}
}

func TestResolveLoginEndpoint_NoSchemeResolves(t *testing.T) {
	got, err := resolveLoginEndpoint("127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolveLoginEndpoint: %v", err)
	}
	if strings.HasPrefix(got, "udp4:") || strings.HasPrefix(got, "udp6:") {
		t.Fatalf("unexpected scheme added: %q", got)
	}
	if !strings.HasPrefix(got, "127.0.0.1:") || strings.HasSuffix(got, ":0") {
		t.Fatalf("port not resolved: %q", got)
	}
}

func TestResolveLoginEndpoint_ConcretePortUnchanged(t *testing.T) {
	const in = "udp4:127.0.0.1:51820"
	got, err := resolveLoginEndpoint(in)
	if err != nil {
		t.Fatalf("resolveLoginEndpoint: %v", err)
	}
	if got != in {
		t.Fatalf("concrete endpoint changed: %q -> %q", in, got)
	}
}

func TestResolveLoginEndpoint_MissingPortErrors(t *testing.T) {
	if _, err := resolveLoginEndpoint("udp4:127.0.0.1"); err == nil {
		t.Fatal("expected error for missing port")
	}
}

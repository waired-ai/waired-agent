package wgnet

import (
	"bytes"
	"strings"
	"testing"
)

func TestBuildUAPIWithTwoPeers(t *testing.T) {
	priv := bytes.Repeat([]byte{0x11}, 32)
	pub1 := bytes.Repeat([]byte{0x22}, 32)
	pub2 := bytes.Repeat([]byte{0x33}, 32)

	got, err := BuildUAPI(UAPIConfig{
		PrivateKey: priv,
		ListenPort: 41820,
		Peers: []UAPIPeer{
			{
				PublicKey:                   pub1,
				AllowedIPs:                  []string{"100.96.0.11/32"},
				Endpoint:                    "127.0.0.1:41821",
				PersistentKeepaliveInterval: 25,
			},
			{
				PublicKey:  pub2,
				AllowedIPs: []string{"100.96.0.12/32"},
				Endpoint:   "127.0.0.1:41822",
			},
		},
	})
	if err != nil {
		t.Fatalf("BuildUAPI: %v", err)
	}
	wantPrefix := "private_key=" + strings.Repeat("11", 32) + "\nlisten_port=41820\n"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("missing self section. got:\n%s", got)
	}
	if !strings.Contains(got, "public_key="+strings.Repeat("22", 32)+"\nallowed_ip=100.96.0.11/32\nendpoint=127.0.0.1:41821\npersistent_keepalive_interval=25\n") {
		t.Fatalf("missing peer1 block. got:\n%s", got)
	}
	if !strings.Contains(got, "public_key="+strings.Repeat("33", 32)+"\nallowed_ip=100.96.0.12/32\nendpoint=127.0.0.1:41822\n") {
		t.Fatalf("missing peer2 block. got:\n%s", got)
	}
	if strings.Contains(got, "persistent_keepalive_interval=0") {
		t.Fatalf("zero keepalive should be omitted. got:\n%s", got)
	}
}

func TestBuildUAPIBadKeyLen(t *testing.T) {
	if _, err := BuildUAPI(UAPIConfig{PrivateKey: []byte{0x01}, ListenPort: 1234}); err == nil {
		t.Fatalf("expected error for short private key")
	}
	if _, err := BuildUAPI(UAPIConfig{
		PrivateKey: bytes.Repeat([]byte{0x01}, 32),
		ListenPort: 1234,
		Peers:      []UAPIPeer{{PublicKey: []byte{0x02}}},
	}); err == nil {
		t.Fatalf("expected error for short peer key")
	}
}

func TestBuildUAPIRequiresEndpoint(t *testing.T) {
	priv := bytes.Repeat([]byte{0x11}, 32)
	pub := bytes.Repeat([]byte{0x22}, 32)
	if _, err := BuildUAPI(UAPIConfig{
		PrivateKey: priv,
		ListenPort: 41820,
		Peers: []UAPIPeer{
			{PublicKey: pub, AllowedIPs: []string{"100.96.0.11/32"}},
		},
	}); err == nil {
		t.Fatalf("expected error when peer has no endpoint")
	}
}

func TestStripUDPScheme(t *testing.T) {
	cases := map[string]string{
		"udp4:127.0.0.1:41820":     "127.0.0.1:41820",
		"udp6:[::1]:41820":         "[::1]:41820",
		"127.0.0.1:41820":          "127.0.0.1:41820",
		"udp4:198.51.100.10:51000": "198.51.100.10:51000",
	}
	for in, want := range cases {
		got, err := normalizeEndpoint(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q -> %q want %q", in, got, want)
		}
	}
}

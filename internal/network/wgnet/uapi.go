package wgnet

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

type UAPIConfig struct {
	PrivateKey []byte
	ListenPort int
	Peers      []UAPIPeer
}

type UAPIPeer struct {
	PublicKey                   []byte
	AllowedIPs                  []string
	Endpoint                    string
	PersistentKeepaliveInterval int
}

// BuildUAPI assembles the wireguard-go UAPI configuration string. The format is
// the same as `wg setconf` IPC (newline separated key=value pairs). Each
// `public_key=` line starts a new peer section.
func BuildUAPI(cfg UAPIConfig) (string, error) {
	if len(cfg.PrivateKey) != 32 {
		return "", fmt.Errorf("private_key must be 32 bytes (got %d)", len(cfg.PrivateKey))
	}
	if cfg.ListenPort < 1 || cfg.ListenPort > 65535 {
		return "", fmt.Errorf("listen_port out of range: %d", cfg.ListenPort)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "private_key=%s\n", hex.EncodeToString(cfg.PrivateKey))
	fmt.Fprintf(&b, "listen_port=%d\n", cfg.ListenPort)

	for i, p := range cfg.Peers {
		if len(p.PublicKey) != 32 {
			return "", fmt.Errorf("peer #%d public_key must be 32 bytes (got %d)", i, len(p.PublicKey))
		}
		if p.Endpoint == "" {
			return "", fmt.Errorf("peer #%d endpoint must not be empty", i)
		}
		ep, err := normalizeEndpoint(p.Endpoint)
		if err != nil {
			return "", fmt.Errorf("peer #%d endpoint: %w", i, err)
		}
		fmt.Fprintf(&b, "public_key=%s\n", hex.EncodeToString(p.PublicKey))
		for _, cidr := range p.AllowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", cidr)
		}
		fmt.Fprintf(&b, "endpoint=%s\n", ep)
		if p.PersistentKeepaliveInterval > 0 {
			fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", p.PersistentKeepaliveInterval)
		}
	}
	return b.String(), nil
}

// BuildUAPIPeerReplace emits a UAPI fragment that atomically replaces the
// device's peer set without touching private_key / listen_port. Use this
// from Engine.UpdatePeers when the Network Map changes.
func BuildUAPIPeerReplace(peers []UAPIPeer) (string, error) {
	var b strings.Builder
	b.WriteString("replace_peers=true\n")
	for i, p := range peers {
		if len(p.PublicKey) != 32 {
			return "", fmt.Errorf("peer #%d public_key must be 32 bytes (got %d)", i, len(p.PublicKey))
		}
		if p.Endpoint == "" {
			return "", fmt.Errorf("peer #%d endpoint must not be empty", i)
		}
		ep, err := normalizeEndpoint(p.Endpoint)
		if err != nil {
			return "", fmt.Errorf("peer #%d endpoint: %w", i, err)
		}
		fmt.Fprintf(&b, "public_key=%s\n", hex.EncodeToString(p.PublicKey))
		for _, cidr := range p.AllowedIPs {
			fmt.Fprintf(&b, "allowed_ip=%s\n", cidr)
		}
		fmt.Fprintf(&b, "endpoint=%s\n", ep)
		if p.PersistentKeepaliveInterval > 0 {
			fmt.Fprintf(&b, "persistent_keepalive_interval=%d\n", p.PersistentKeepaliveInterval)
		}
	}
	return b.String(), nil
}

// normalizeEndpoint strips the optional "udp4:" or "udp6:" prefix used in the
// network.yaml endpoints array. wireguard-go expects a bare host:port literal.
func normalizeEndpoint(s string) (string, error) {
	switch {
	case strings.HasPrefix(s, "udp4:"):
		return strings.TrimPrefix(s, "udp4:"), nil
	case strings.HasPrefix(s, "udp6:"):
		return strings.TrimPrefix(s, "udp6:"), nil
	case s == "":
		return "", errors.New("endpoint must not be empty")
	default:
		return s, nil
	}
}

package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// OverlayDialer dials a TCP connection to an overlay IP/port. wgnet.Engine
// satisfies this interface via DialOverlayTCP.
type OverlayDialer interface {
	DialOverlayTCP(ctx context.Context, ip netip.Addr, port uint16) (net.Conn, error)
}

// Client speaks the overlay-side HTTP service from one daemon to another. It
// is designed to be stuffed into an http.Client's Transport so the rest of
// the daemon can use stdlib semantics.
type Client struct {
	http *http.Client
}

// NewClient builds a Client whose http.Transport dials peers via the supplied
// OverlayDialer. The host portion of the URL is treated as the overlay IP.
func NewClient(dialer OverlayDialer, requestTimeout time.Duration) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ip, err := netip.ParseAddr(host)
			if err != nil {
				return nil, fmt.Errorf("overlay dial: %w", err)
			}
			p, err := netip.ParseAddrPort(addr)
			if err != nil {
				return nil, fmt.Errorf("overlay dial: %w (host=%s port=%s)", err, host, port)
			}
			return dialer.DialOverlayTCP(ctx, ip, p.Port())
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: requestTimeout,
	}
	if requestTimeout == 0 {
		requestTimeout = 5 * time.Second
	}
	return &Client{
		http: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
		},
	}
}

// Ping fetches /waired/v1/ping from a peer overlay endpoint and returns the
// raw response together with the round-trip duration.
func (c *Client) Ping(ctx context.Context, ip netip.Addr, port uint16) (PingResponse, time.Duration, error) {
	url := fmt.Sprintf("http://%s/waired/v1/ping", netip.AddrPortFrom(ip, port).String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return PingResponse{}, 0, err
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return PingResponse{}, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.DebugContext(ctx, "overlay ping non-200", "addr", url, "status", resp.StatusCode)
		return PingResponse{}, 0, fmt.Errorf("peer returned status %d", resp.StatusCode)
	}
	var body PingResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return PingResponse{}, 0, err
	}
	rtt := time.Since(start)
	slog.DebugContext(ctx, "overlay ping ok", "device", body.Device, "rtt_ms", rtt.Milliseconds())
	return body, rtt, nil
}

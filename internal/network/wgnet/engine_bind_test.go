package wgnet_test

import (
	"errors"
	"net"
	"net/netip"
	"testing"

	"github.com/waired-ai/waired-agent/internal/network/wgnet"
)

// TestNewEngineBindFailureIsTagged pins the classification the ephemeral
// fallback depends on (waired-agent#318): a failure to open the UDP
// socket must come back wrapped in ErrBindFailed, so the caller can tell
// "this port is unusable" from "the device could not be built at all"
// and retry accordingly.
//
// The reproducer here is a port already held by another socket. The real
// trigger on Windows is different — a port inside a winnat/Hyper-V
// excluded UDP range — but both surface at the same stage, which is
// exactly why the classification is by stage and not by errno.
func TestNewEngineBindFailureIsTagged(t *testing.T) {
	held, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	defer held.Close()
	port := held.LocalAddr().(*net.UDPAddr).Port

	priv, _ := genKey(t)
	eng, err := wgnet.NewEngine(wgnet.Config{
		SelfName:       "conflict",
		SelfOverlayIP:  netip.MustParseAddr("100.96.0.30"),
		SelfPrivateKey: priv,
		ListenPort:     port,
	})
	if err == nil {
		eng.Close()
		t.Skip("this platform allows two UDP sockets on the same port; " +
			"the ErrBindFailed path is covered by the table test in cmd/waired-agent")
	}
	if !errors.Is(err, wgnet.ErrBindFailed) {
		t.Fatalf("err = %v, want it to wrap ErrBindFailed", err)
	}
}

// TestEngineListenPortReportsEphemeral pins the other half: asking for
// port 0 must yield a real, discoverable port. Without this the agent
// could fall back to an ephemeral port and then advertise 0 to its
// peers.
func TestEngineListenPortReportsEphemeral(t *testing.T) {
	priv, _ := genKey(t)
	eng, err := wgnet.NewEngine(wgnet.Config{
		SelfName:       "ephemeral",
		SelfOverlayIP:  netip.MustParseAddr("100.96.0.31"),
		SelfPrivateKey: priv,
		ListenPort:     0,
	})
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	defer eng.Close()

	port, err := eng.ListenPort()
	if err != nil {
		t.Fatalf("ListenPort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("ListenPort = %d, want a real bound port", port)
	}
}

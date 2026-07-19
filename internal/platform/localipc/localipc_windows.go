//go:build windows

package localipc

import (
	"fmt"
	"net"

	"github.com/Microsoft/go-winio"
)

// pipeSDDL is the named-pipe security descriptor. It grants GENERIC_ALL to
// LocalSystem (SY) and Administrators (BA) — the daemon runs as
// LocalSystem — and to local Interactive users (IU) so any desktop
// session's tray/CLI can connect. IU excludes network logons, so the pipe
// is NOT reachable over SMB (\\host\pipe\waired-mgmt) — only from a local
// interactive session. Owner and group are Administrators. There is no
// per-client token check beyond this ACL (waired#838): the boundary is the
// transport, and every local interactive user legitimately drives their
// own tray on a system-wide install.
const pipeSDDL = "O:BAG:BAD:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;IU)"

// listen creates the named pipe pipeName as a byte-stream (MessageMode
// off) so net/http can serve over it, protected by pipeSDDL.
func listen(pipeName string) (net.Listener, error) {
	ln, err := winio.ListenPipe(pipeName, &winio.PipeConfig{
		SecurityDescriptor: pipeSDDL,
	})
	if err != nil {
		return nil, fmt.Errorf("localipc: listen pipe %s: %w", pipeName, err)
	}
	return ln, nil
}

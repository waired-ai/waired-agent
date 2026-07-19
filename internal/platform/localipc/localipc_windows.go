//go:build windows

package localipc

import (
	"errors"
	"fmt"
	"net"
	"os"

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
//
// Unlike the unix side there is no stale node to clear and no
// steal-a-live-endpoint hazard (waired#81) to defend against by hand: winio
// creates the FIRST instance of a pipe with NT disposition FILE_CREATE, so
// a second listener on the same name fails with STATUS_OBJECT_NAME_COLLISION
// (surfacing as ERROR_ALREADY_EXISTS) rather than shadowing the first. The
// kernel does that check atomically, so this path has none of the unix
// probe's TOCTOU window; we only translate the error. winio also sets
// FILE_PIPE_REJECT_REMOTE_CLIENTS unconditionally, which blocks SMB reach
// at the driver level independently of pipeSDDL's IU term.
func listen(pipeName string) (net.Listener, error) {
	ln, err := winio.ListenPipe(pipeName, &winio.PipeConfig{
		SecurityDescriptor: pipeSDDL,
	})
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("localipc: %s: %w", pipeName, ErrEndpointInUse)
		}
		return nil, fmt.Errorf("localipc: listen pipe %s: %w", pipeName, err)
	}
	return ln, nil
}

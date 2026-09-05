package management

import (
	"net/http"

	"github.com/waired-ai/waired-agent/internal/notice"
)

// NoticeProvider returns the notices the daemon is publishing right now.
// Implemented by the agent (cmd/waired-agent), which owns the registry
// the producers publish into.
type NoticeProvider interface {
	Notices() []notice.Notice
}

// NoticesResponse is the body of GET /waired/v1/notices. Notices is
// always present, empty rather than null, so a reader can render an
// empty list without distinguishing the two.
type NoticesResponse struct {
	Notices []notice.Notice `json:"notices"`
}

// WithNotices attaches a NoticeProvider so the server exposes
// GET /waired/v1/notices. Pass nil to disable.
//
// A route of its own rather than a field on an existing response,
// because no existing one is read by all three surfaces that show
// notices, and because the obvious candidate is the wrong place twice
// over: Status is a hot path with shell-script consumers (see
// WithIdentity, which was kept off it for the same reason), and
// `waired status` prints that document verbatim, so a notice there would
// appear once as raw JSON and again as a rendered line.
func (s *Server) WithNotices(p NoticeProvider) *Server {
	s.notices = p
	return s
}

// handleNotices serves the current notice list.
//
// The route is deliberately NOT in socket.go's tcpReadRoutes: that list
// is the routes non-Go consumers call, and all three readers here — the
// tray, `waired doctor` and `waired status` — reach the daemon over the
// local IPC socket already.
func (s *Server) handleNotices(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET only"})
		return
	}
	ns := s.notices.Notices()
	if ns == nil {
		ns = []notice.Notice{}
	}
	writeJSON(w, http.StatusOK, NoticesResponse{Notices: notice.Clamp(ns)})
}

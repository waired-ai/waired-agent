package management

import (
	"net/http"

	"github.com/waired-ai/waired-agent/internal/inferencemesh"
)

// InferenceMeshProvider is implemented by the agent's
// inferencemesh.Aggregator. The handler holds nothing else from the
// aggregator; it simply asks for the current snapshot on every GET.
type InferenceMeshProvider interface {
	Snapshot() inferencemesh.Snapshot
}

// handleInferenceMesh serves GET /waired/v1/inference/mesh — the
// loopback-only diagnostic snapshot consumed by `waired claude
// --waired-diagnose` and (eventually) the tray's mesh visualisation.
func (s *Server) handleInferenceMesh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.infMesh == nil {
		http.Error(w, "inference mesh provider not attached", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, s.infMesh.Snapshot())
}

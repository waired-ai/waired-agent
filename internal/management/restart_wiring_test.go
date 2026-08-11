package management

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// PRODUCT CONTRACT (waired-agent#684): this package does not know how to
// restart the process, and says so rather than guessing.
//
// It used to carry its own per-OS restart implementation as the default
// when no scheduler was wired — and those two copies drifting IS #684:
// the Windows one called os.Exit(1) from inside the service process, so
// the SCM read every model switch as a hard crash. The mechanism now
// lives only in internal/platform/service.
//
// Importing that package HERE would be the obvious move and is the wrong
// one: it drags the service-manager and enrollment packages into the
// routing harness's dependency closure, which
// scripts/ci/routing-sentinel-paths-guard.sh caught on the first attempt.
// cmd/waired-agent wires the scheduler instead.
//
// A 202 for a restart that will never happen is the failure this
// replaces: the caller records a switch as applied and the model never
// changes.
func TestPreferredModel_RefusesWhenNoRestartMechanismIsWired(t *testing.T) {
	dir := t.TempDir()
	inf := &fakeInference{}
	s := New(stubStatus{}, stubPinger{}).WithInference(inf).WithCatalog(&CatalogConfig{
		PreferencePath: filepath.Join(dir, "preferred-model.json"),
		ManifestsFn:    func() ([]catalog.Manifest, error) { return catalogFixture(), nil },
		// RestartScheduler deliberately nil, and ApplyModelSwitch nil so
		// the request reaches the restart fallback.
	})

	w := doPostJSON(t, s, "/waired/v1/inference/preferred-model",
		map[string]string{"model_id": catalogFixture()[0].ModelID})

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body=%s, want 500", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "restart_unavailable") {
		t.Errorf("body = %s, want the restart_unavailable code", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"will_restart":true`) {
		t.Error("a daemon that cannot restart must not report will_restart")
	}
}

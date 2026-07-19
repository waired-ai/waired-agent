package intercept

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/gateway"
)

// TestDirectiveIdsInSyncWithGateway guards the hand-duplicated directive id
// literals: the intercept honours (model_rewrite.go) and advertises-on-passthrough
// (models_directives.go) exactly the ids the gateway advertises on the
// local-serving path (internal/gateway/anthropic_models.go). They are duplicated
// — not shared — to keep this fail-open package stdlib-only, so nothing but this
// test stops them silently drifting. Drift would make the picker show an id the
// intercept can no longer force a route for (local) or rewrite for upstream
// (cloud), with no other test catching it.
func TestDirectiveIdsInSyncWithGateway(t *testing.T) {
	if wairedLocalModel != gateway.ModelWairedLocal {
		t.Errorf("local directive id drift: intercept %q != gateway %q", wairedLocalModel, gateway.ModelWairedLocal)
	}
	if wairedCloudModel != gateway.ModelWairedCloud {
		t.Errorf("cloud directive id drift: intercept %q != gateway %q", wairedCloudModel, gateway.ModelWairedCloud)
	}
}

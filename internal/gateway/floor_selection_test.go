package gateway

import (
	"errors"
	"net/http"
	"testing"

	"github.com/waired-ai/waired-agent/internal/router"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// TestSelectionFloorRowsMatchTheResponders.
//
// PRODUCT CONTRACT (the doc comment on selectionStatus): every row here
// has to be the status the responders WRITE, because ev.Status is what
// reaches the event ring as the status the client received.
//
// The floor had no row at all, so it was answered by whatever it
// wrapped: model_not_served for a not-ready base, and — worse — 503 for
// a floor over an engine-less requester, while both responders wrote 404
// (waired-agent#1178). No test in this package exercised the floor arm
// before this one.
func TestSelectionFloorRowsMatchTheResponders(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			// The rc5 shape: this device serves a ready model the floor
			// excludes.
			name: "over a ready local model",
			err: &router.SizeFloorError{
				Err:               &router.ModelNotReadyError{ModelID: "qwen3.6-35b-a3b", State: "ready"},
				Floor:             hostfit.ModelSizeLarge,
				LocalArmOnlyFloor: true,
			},
		},
		{
			// The engine-less requester the wrapper exists for: the miss
			// arrives as ErrLocalInferenceOff, and the status rows used to
			// answer 503 about it while the responders wrote 404.
			name: "over an engine-less requester",
			err:  &router.SizeFloorError{Err: router.ErrLocalInferenceOff, Floor: hostfit.ModelSizeMedium},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectionErrorReason(tc.err); got != LocalErrorModelTooSmall {
				t.Errorf("selectionErrorReason = %q, want %q — the journal named the wrapped error",
					got, LocalErrorModelTooSmall)
			}
			if got := selectionStatus(tc.err); got != http.StatusNotFound {
				t.Errorf("selectionStatus = %d, want %d — both responders write 404 for the floor",
					got, http.StatusNotFound)
			}
		})
	}

	// The classification is not widened: an ordinary miss still answers
	// about itself.
	plain := &router.ModelNotReadyError{ModelID: "m", State: "ready"}
	if got := selectionErrorReason(plain); got == LocalErrorModelTooSmall {
		t.Error("an ordinary not-ready miss must not read as a floor exclusion")
	}
	if !errors.Is(router.ErrLocalInferenceOff, router.ErrLocalInferenceOff) {
		t.Fatal("sanity")
	}
}

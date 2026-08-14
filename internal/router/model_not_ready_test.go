package router

import (
	"errors"
	"fmt"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
)

// Telling "on its way" from "nobody has it" (waired-agent#788).
//
// Measured: `waired claude route waired` plus `--model qwen3.5-9b`, a
// model whose weights sat on another host's disk but which no host
// advertised. The gateway answered 503 every 30-40 s and the Claude CLI
// backed off in silence; the operator killed it after 327 s of blank
// terminal. Under route=auto the same request produced a visible error
// in 2.5 s, because the intercept fell back and the real API said 404.

func TestModelIsArriving_CoversEveryLocalState(t *testing.T) {
	// Every state internal/catalog defines, so a new one cannot be added
	// without a decision being made about it here.
	for _, tc := range []struct {
		state string
		want  bool
	}{
		{catalog.ModelStateQueued, true},
		{catalog.ModelStateDownloading, true},
		{catalog.ModelStateVerifying, true},
		{catalog.ModelStateNotPresent, false},
		{catalog.ModelStateFailed, false},
		{catalog.ModelStateEvicted, false},
		// Not a contradiction: "ready on disk" reaching a not-ready
		// branch means something else disqualified the endpoint, and no
		// download is going to change that.
		{catalog.ModelStateReady, false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			err := modelNotReady("qwen3.5-9b", tc.state, "")
			if got := ModelIsArriving(err); got != tc.want {
				t.Errorf("ModelIsArriving(state=%q) = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}

// A caller that cannot see a state has no evidence the wait would end,
// and answering "retry" on no evidence is the defect.
func TestModelIsArriving_NeedsAState(t *testing.T) {
	if ModelIsArriving(ErrModelNotReady) {
		t.Error("the bare sentinel was read as a model on its way")
	}
	if ModelIsArriving(nil) {
		t.Error("nil was read as a model on its way")
	}
	if ModelIsArriving(errors.New("something else")) {
		t.Error("an unrelated error was read as a model on its way")
	}
	// Still true through a wrap, which is how the gateway receives it.
	arriving := fmt.Errorf("selection failed: %w",
		modelNotReady("qwen3.5-9b", catalog.ModelStateDownloading, "routing=peer-only"))
	if !ModelIsArriving(arriving) {
		t.Error("a wrapped downloading model was not recognised")
	}
}

// The typed error must keep every existing sentinel comparison working —
// the same rule PinnedPeerUnreachableError follows.
func TestModelNotReadyError_IsStillTheSentinel(t *testing.T) {
	err := modelNotReady("qwen3.5-9b", catalog.ModelStateNotPresent, "routing=peer-only, no mesh candidate")
	if !errors.Is(err, ErrModelNotReady) {
		t.Fatal("errors.Is(err, ErrModelNotReady) is false — every existing comparison just broke")
	}
	// The wire text is unchanged from the sentinel path it replaced: this
	// string reaches the client's error body and an operator's journal.
	want := `router: model is not in ready state on disk: "qwen3.5-9b" state="not_present" (routing=peer-only, no mesh candidate)`
	if got := err.Error(); got != want {
		t.Errorf("message drifted\n got: %s\nwant: %s", got, want)
	}
	plain := modelNotReady("qwen3.5-9b", catalog.ModelStateNotPresent, "")
	if got, want := plain.Error(), `router: model is not in ready state on disk: "qwen3.5-9b" state="not_present"`; got != want {
		t.Errorf("message drifted\n got: %s\nwant: %s", got, want)
	}
}

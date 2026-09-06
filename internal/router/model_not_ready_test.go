package router

import (
	"errors"
	"fmt"
	"testing"

	"github.com/waired-ai/waired-agent/internal/catalog"
	"github.com/waired-ai/waired-agent/internal/runtime/state"
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

// State is THIS host's local state, so it is only evidence about this turn
// on a branch that would have run here (waired-agent#1252).
//
// PIN: product contract. A host that happened to be downloading a model
// answered every peer-only, pinned and "Waired public share" refusal with
// 503 + Retry-After, so the message naming the real reason never reached
// the client — waired-agent#788's defect, reintroduced through a field
// that means something different on the branch that set it.
func TestModelIsArriving_NeedsTheBranchToHaveLookedLocally(t *testing.T) {
	s := NewSelector(Inputs{})
	for _, tc := range []struct {
		name string
		mk   func(state string) error
		want bool
	}{
		{
			name: "the local branch",
			mk:   func(st string) error { return modelNotReady("qwen3.5-9b", st, "") },
			want: true,
		},
		{
			name: "peer-preferred, which would have taken a local candidate",
			mk: func(st string) error {
				return s.meshMissAfterLocal("qwen3.5-9b", st, "routing=peer-preferred")
			},
			want: true,
		},
		{
			name: "a mesh branch that refused to run here",
			mk:   func(st string) error { return meshMiss("qwen3.5-9b", st, "routing=peer-only") },
			want: false,
		},
		{
			name: "the public entry, which carries no local state at all",
			mk:   func(string) error { return publicShareDeclined("no public machine is reachable right now") },
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, st := range []string{
				catalog.ModelStateQueued, catalog.ModelStateDownloading, catalog.ModelStateVerifying,
			} {
				if got := ModelIsArriving(tc.mk(st)); got != tc.want {
					t.Errorf("ModelIsArriving(state=%q) = %v, want %v", st, got, tc.want)
				}
			}
		})
	}
}

// The whole design in one assertion: a peer-only turn refused while this
// computer happens to be downloading a model must NOT be reported as a
// wait, and a peer-PREFERRED one still must.
//
// PIN: product contract (waired-agent#1252). The middle row is the
// regression guard — peer-preferred would have run on those weights, so
// their arrival really is what the client is waiting for.
func TestPeerOnlyMiss_DoesNotBorrowALocalDownload(t *testing.T) {
	downloading := func() catalog.State {
		st := emptyState()
		st.Models["qwen3-8b-instruct"] = catalog.ModelState{
			VariantID: "q4-gguf",
			OllamaTag: "qwen3:8b-q4_K_M",
			State:     catalog.ModelStateDownloading,
		}
		return st
	}
	for _, tc := range []struct {
		name       string
		mode       state.RoutingMode
		publicOnly bool
		want       bool
	}{
		{name: "peer-only", mode: state.RoutingModePeerOnly, want: false},
		{name: "peer-preferred", mode: state.RoutingModePeerPreferred, want: true},
		// The public entry carries no local state at all, so this row
		// pins the SHAPE rather than the flag; the flag itself is pinned
		// per-constructor above. Pinned routing is not driven from here:
		// an absent pin answers ErrPinnedPeerUnreachable long before the
		// mesh miss, and its meshMiss use is the same constructor row.
		{name: "the public entry", mode: state.RoutingModePeerOnly, publicOnly: true, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := publicSelector(t, allowAll())
			s.in.LocalState = downloading()
			s.in.RoutingMode = tc.mode
			s.in.PublicOnly = tc.publicOnly
			_, err := s.SelectK(t.Context(), Request{Model: "waired/default"}, 5)
			if err == nil {
				t.Fatal("an empty mesh with no local candidate must refuse")
			}
			if got := ModelIsArriving(err); got != tc.want {
				t.Errorf("ModelIsArriving(%v) = %v, want %v", err, got, tc.want)
			}
		})
	}
}

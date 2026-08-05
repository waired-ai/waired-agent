package main

import (
	"flag"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

func boolPtr(v bool) *bool { return &v }

// TestPlanInitialInference is the table the conversion this replaces
// never had. `if !cfgRoot.Inference.Enabled { *disableInference = true }`
// was three lines inline in run(), untested, and it is what turned a
// below-recommended-spec install into a state with no product-side exit:
// the subsystem was never built, so every route that could turn local
// inference back on was unregistered (#465, waired-ai/waired#1056).
//
// The precedence is the one desired-share already uses for the same
// shape of decision — agentconfig is the install-time default, the
// persisted runtime file is the choice made since — plus an explicit
// boot flag on top, which is the only signal newer than either.
func TestPlanInitialInference(t *testing.T) {
	cases := []struct {
		name        string
		cfgEnabled  bool
		persisted   state.InferenceState
		explicit    *bool
		wantState   state.InferenceState
		wantPersist bool
	}{
		{
			name:       "a pristine install follows agent.json",
			cfgEnabled: true,
			wantState:  state.InferenceEnabled,
		},
		{
			// The below-recommended-spec default. It is a DEFAULT now:
			// the routes, the tray group and the wizard all stay alive,
			// and turning it on writes desired-inference.
			name:       "below recommended spec starts off, and does not persist that",
			cfgEnabled: false,
			wantState:  state.InferenceDisabled,
		},
		{
			name:       "a persisted choice outranks agent.json",
			cfgEnabled: false,
			persisted:  state.InferenceEnabled,
			wantState:  state.InferenceEnabled,
		},
		{
			name:       "a persisted disable outranks agent.json too",
			cfgEnabled: true,
			persisted:  state.InferenceDisabled,
			wantState:  state.InferenceDisabled,
		},
		{
			// --inference-enabled / WAIRED_INFERENCE_ENABLED used to win
			// for exactly one boot and change nothing, which is why the
			// recovery path the docs named never worked. Saying it is
			// now the same act as saying it from the CLI or the tray.
			name:        "an explicit flag wins and is persisted",
			cfgEnabled:  false,
			explicit:    boolPtr(true),
			wantState:   state.InferenceEnabled,
			wantPersist: true,
		},
		{
			name:        "an explicit flag overrides a persisted choice",
			cfgEnabled:  true,
			persisted:   state.InferenceDisabled,
			explicit:    boolPtr(true),
			wantState:   state.InferenceEnabled,
			wantPersist: true,
		},
		{
			name:        "an explicit off is a durable choice as well",
			cfgEnabled:  true,
			explicit:    boolPtr(false),
			wantState:   state.InferenceDisabled,
			wantPersist: true,
		},
		{
			// No write when the flag agrees with what is already on
			// disk: a unit file carrying the flag would otherwise
			// rewrite the file on every restart for no reason.
			name:       "an explicit flag that agrees writes nothing",
			cfgEnabled: true,
			persisted:  state.InferenceEnabled,
			explicit:   boolPtr(true),
			wantState:  state.InferenceEnabled,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := planInitialInference(tc.cfgEnabled, tc.persisted, tc.explicit)
			if got.State != tc.wantState {
				t.Errorf("State = %q, want %q", got.State, tc.wantState)
			}
			if got.Persist != tc.wantPersist {
				t.Errorf("Persist = %v, want %v", got.Persist, tc.wantPersist)
			}
		})
	}
}

// TestResolveInferenceIntentEnablement pins the raw enablement signal
// planInitialInference reads. Skip conflates three different reasons to
// not auto-select a model (inference off, an operator's own preferred
// model, --disable-inference), so the boot decision cannot use it: only
// one of those three is a statement about whether inference runs.
//
// The flag set is the REAL one (RegisterInferenceFlags), so a rename
// cannot silently move a flag out of this function's reach.
func TestResolveInferenceIntentEnablement(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		environ []string
		want    *bool
	}{
		{name: "nothing said", want: nil},
		{name: "flag on", args: []string{"--inference-enabled=true"}, want: boolPtr(true)},
		{name: "flag off", args: []string{"--inference-enabled=false"}, want: boolPtr(false)},
		{name: "env on", environ: []string{"WAIRED_INFERENCE_ENABLED=true"}, want: boolPtr(true)},
		{name: "env off", environ: []string{"WAIRED_INFERENCE_ENABLED=false"}, want: boolPtr(false)},
		{
			name:    "flag beats env on the same axis",
			args:    []string{"--inference-enabled=true"},
			environ: []string{"WAIRED_INFERENCE_ENABLED=false"},
			want:    boolPtr(true),
		},
		{
			// --disable-inference is the operator's transient kill
			// switch, not a durable choice, so it must not reach the
			// persisted state. It still forces Skip.
			name: "--disable-inference says nothing about the durable state",
			args: []string{"--disable-inference"},
			want: nil,
		},
		{
			name: "an engine-wiring flag says nothing",
			args: []string{"--inference-ollama-port=11500"},
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := bootFlagSet(t)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			disable := false
			fs.Visit(func(f *flag.Flag) {
				if f.Name == "disable-inference" {
					disable = f.Value.String() == "true"
				}
			})
			got := resolveInferenceIntent(disable, fs, tc.environ).Enablement
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("Enablement = %v, want nil (nothing was said)", *got)
			case tc.want != nil && got == nil:
				t.Errorf("Enablement = nil, want %v", *tc.want)
			case tc.want != nil && got != nil && *tc.want != *got:
				t.Errorf("Enablement = %v, want %v", *got, *tc.want)
			}
		})
	}
}

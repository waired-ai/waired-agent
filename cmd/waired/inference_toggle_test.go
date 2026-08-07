package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// `waired inference on|off` is the CLI half of #465's opt-in. Before it
// existed, `waired init` printed "change it later with `waired inference
// on`" for a command that was never built — one of two remediation lines
// naming something that does not exist (the other said `waired models
// use`).
//
// The daemon-first / persist-on-refused shape is `share`'s
// (runShareTransition), and it matters more here: the machine this is
// aimed at is one whose local inference is off, where a daemon that is
// not answering is exactly what the user is trying to fix.

func TestRunInferenceOn_HitsTheDaemonWhenReachable(t *testing.T) {
	for _, tc := range []struct {
		verb string
		want string
	}{
		{verb: "on", want: "/waired/v1/inference/enable"},
		{verb: "off", want: "/waired/v1/inference/disable"},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			dir := t.TempDir()
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"state":"enabled","desired_state":"enabled"}`))
			}))
			defer srv.Close()

			var err error
			captureStdout(t, func() {
				err = runInference([]string{tc.verb, "--mgmt", srv.URL, "--state-dir", dir})
			})
			if err != nil {
				t.Fatalf("runInference %s: %v", tc.verb, err)
			}
			if got != tc.want {
				t.Errorf("daemon hit %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunInferenceOn_PersistsWhenTheDaemonIsUnreachable(t *testing.T) {
	for _, tc := range []struct {
		verb  string
		seed  state.InferenceState
		want  state.InferenceState
		label string
	}{
		{verb: "on", seed: state.InferenceDisabled, want: state.InferenceEnabled},
		{verb: "off", want: state.InferenceDisabled},
	} {
		t.Run(tc.verb, func(t *testing.T) {
			dir := t.TempDir()
			if tc.seed != "" {
				if err := state.WriteDesiredInferenceState(dir, tc.seed); err != nil {
					t.Fatal(err)
				}
			}
			addr, err := newClosedTCPAddr()
			if err != nil {
				t.Fatal(err)
			}

			captureStdout(t, func() {
				err = runInference([]string{tc.verb, "--mgmt", "http://" + addr, "--state-dir", dir})
			})
			if err != nil {
				t.Fatalf("runInference %s: %v", tc.verb, err)
			}
			got, rerr := state.ReadDesiredInferenceState(dir)
			if rerr != nil {
				t.Fatalf("ReadDesiredInferenceState: %v", rerr)
			}
			if got != tc.want {
				t.Errorf("desired-inference = %q, want %q — the choice has to survive "+
					"a daemon that is not answering", got, tc.want)
			}
		})
	}
}

// TestRunInferenceStatus_SaysHowToTurnItBackOn: the state this command
// exists to report is the one a user needs a way out of, so reporting it
// without the way out would repeat #465's original defect in a new place.
func TestRunInferenceStatus_SaysHowToTurnItBackOn(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
		// notWant is for the lines that must NOT appear: a claim about
		// why something is off is wrong to make when it is not the reason.
		notWant []string
	}{
		{
			name: "off names the command that turns it on",
			body: `{"subsystem_state":"disabled","desired_state":"disabled"}`,
			want: []string{"Local inference: off", "waired inference on"},
		},
		{
			name: "on reports the engine",
			body: `{"subsystem_state":"ready","desired_state":"enabled"}`,
			want: []string{"Local inference: on", "Inference engine: ready"},
		},
		{
			// An older daemon answers /inference/status without the
			// field. Saying nothing beats inventing a state.
			name: "an older daemon is not guessed at",
			body: `{"subsystem_state":"ready"}`,
			want: []string{"Local inference: unknown"},
		},
		{
			// #496: when Waired is the one who decided, it says so. Until
			// this shipped the only record of that decision was a line in
			// the daemon log, so an off state read as a setting somebody
			// forgot rather than an answer the machine worked out.
			name: "off because the machine measured too slow says so",
			body: `{"subsystem_state":"disabled","desired_state":"disabled",` +
				`"host_speed":{"turn_seconds":68.4,"budget_seconds":45,"turned_inference_off":true}}`,
			want: []string{
				"Local inference: off",
				"One coding question would take about 68.4 s here; Waired starts local AI off above 45 s.",
				"This computer can still use the AI running on your other computers.",
				"waired inference on",
			},
		},
		{
			// The same measurement, but somebody turned the toggle off
			// themselves. Telling them their machine is too slow would be
			// a story about a decision they made, not one Waired made.
			name: "off by hand is not blamed on the measurement",
			body: `{"subsystem_state":"disabled","desired_state":"disabled",` +
				`"host_speed":{"turn_seconds":68.4,"budget_seconds":45}}`,
			notWant: []string{"would take about"},
			want:    []string{"Local inference: off", "waired inference on"},
		},
		{
			name: "on reports what one question costs here",
			body: `{"subsystem_state":"ready","desired_state":"enabled",` +
				`"host_speed":{"turn_seconds":4.5,"budget_seconds":45}}`,
			want: []string{"Local inference: on", "One coding question takes about 4.5 s on this computer."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			out := captureStdout(t, func() {
				if err := runInference([]string{"status", "--mgmt", srv.URL}); err != nil {
					t.Fatalf("runInference status: %v", err)
				}
			})
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output %q does not contain %q", out, want)
				}
			}
			for _, unwanted := range tc.notWant {
				if strings.Contains(out, unwanted) {
					t.Errorf("output %q contains %q", out, unwanted)
				}
			}
		})
	}
}

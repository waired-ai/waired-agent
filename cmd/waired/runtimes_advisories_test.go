package main

import (
	"bytes"
	"strings"
	"testing"

	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// PRODUCT CONTRACT, ratified by waired-agent#957: the CLI must not tell an
// operator the engine cannot start when it can.
//
// The defect was not in the advisory strings — those were table-tested — but
// in the heading printed above them, which no test reached. `waired runtimes
// install vllm` on a fully provisioned host (g++, a host CUDA toolkit, and
// the pin set's own bundled-CUDA skew) printed:
//
//	This host cannot start the engine yet:
//	  - the CUDA bundled inside the venv is inconsistent: ...
//
// and the engine then started and served four e2e lanes green.
func TestRenderVLLMAdvisories(t *testing.T) {
	const blockerText = "no C++ compiler (g++) on this host. ... the engine will not start."
	const noteText = "the CUDA bundled inside the venv is inconsistent: ... This is harmless while a host CUDA toolkit is present"

	blocker := infruntime.VLLMAdvisory{Blocking: true, Text: blockerText}
	note := infruntime.VLLMAdvisory{Text: noteText}

	for _, tc := range []struct {
		name      string
		in        []infruntime.VLLMAdvisory
		wantLines []string
		denyLines []string
	}{
		{
			name: "nothing to say prints nothing",
			in:   nil,
		},
		{
			// The #957 case itself.
			name:      "a non-blocking advisory alone does not claim the engine cannot start",
			in:        []infruntime.VLLMAdvisory{note},
			wantLines: []string{"The engine will start. Worth knowing:", "  - " + noteText},
			denyLines: []string{"This host cannot start the engine yet:"},
		},
		{
			name:      "a blocking advisory still says the engine cannot start",
			in:        []infruntime.VLLMAdvisory{blocker},
			wantLines: []string{"This host cannot start the engine yet:", "  - " + blockerText},
			denyLines: []string{"The engine will start. Worth knowing:"},
		},
		{
			// Both at once: the reassurance must not appear beside a blocker,
			// or one heading contradicts the other on the same screen.
			name:      "with a blocker present the notes do not promise a start",
			in:        []infruntime.VLLMAdvisory{blocker, note},
			wantLines: []string{"This host cannot start the engine yet:", "Also worth knowing:"},
			denyLines: []string{"The engine will start. Worth knowing:"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderVLLMAdvisories(&buf, tc.in)
			got := buf.String()

			if len(tc.in) == 0 && strings.TrimSpace(got) != "" {
				t.Fatalf("no advisories but printed:\n%s", got)
			}
			for _, want := range tc.wantLines {
				if !strings.Contains(got, want) {
					t.Errorf("output does not contain %q:\n%s", want, got)
				}
			}
			for _, deny := range tc.denyLines {
				if strings.Contains(got, deny) {
					t.Errorf("output must not contain %q:\n%s", deny, got)
				}
			}
		})
	}
}

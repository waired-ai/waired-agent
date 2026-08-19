package management

import (
	"testing"

	"github.com/waired-ai/waired-agent/internal/hardware"
)

// TestInferenceCatalog_ReportsWhetherTheEngineIsHere pins #852's wire
// half.
//
// catalogEngine answers "which engine would this host serve", and with
// nothing committed it falls back to the auto-picker, which demotes vLLM
// to ollama when no venv is present. On a host with NO engine at all it
// therefore names ollama, and every family is judged against it — a
// verdict by an engine that is not there, with nothing on the response
// saying so.
//
// The engine name stays what it is (emptying it would turn every row
// into "no variant for this engine" and destroy the verdicts, which are
// true about what this computer would run once an engine is installed).
// What is added is the separate fact.
func TestInferenceCatalog_ReportsWhetherTheEngineIsHere(t *testing.T) {
	cases := []struct {
		name      string
		runtimes  map[string]RuntimeStatus
		wantNil   bool
		wantValue bool
	}{
		{
			// The observed host: adapters registered, engine absent.
			name:      "no engine on the host",
			runtimes:  map[string]RuntimeStatus{"ollama": {Name: "ollama", Installed: false}},
			wantValue: false,
		},
		{
			name:      "engine installed",
			runtimes:  map[string]RuntimeStatus{"ollama": {Name: "ollama", Installed: true, Version: "0.32.13"}},
			wantValue: true,
		},
		{
			// A provider that reports no runtimes cannot answer, and
			// unknown must not read as absent — every surface then keeps
			// its pre-#852 rendering.
			name:     "provider reports no runtimes",
			runtimes: nil,
			wantNil:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inf := &fakeInference{
				hwProfile: hardware.Profile{RAMTotalGB: 32},
				canned:    InferenceStatus{Runtimes: tc.runtimes},
			}
			s := newCatalogTestServer(t, inf, t.TempDir())
			w, got := doGet(t, s, "/waired/v1/inference/catalog")
			if w.Code != 200 {
				t.Fatalf("status = %d", w.Code)
			}
			if tc.wantNil {
				if got.EngineInstalled != nil {
					t.Fatalf("EngineInstalled = %v, want nil (unknown)", *got.EngineInstalled)
				}
				return
			}
			if got.EngineInstalled == nil {
				t.Fatalf("EngineInstalled = nil, want %v", tc.wantValue)
			}
			if *got.EngineInstalled != tc.wantValue {
				t.Errorf("EngineInstalled = %v, want %v", *got.EngineInstalled, tc.wantValue)
			}
			// Whatever the answer, the rows and the engine name are
			// unchanged: this field adds context, it does not withdraw
			// the catalog.
			if got.Engine == "" {
				t.Error("Engine went empty; the rows would lose their verdicts")
			}
			if len(got.Families) != len(catalogFixture()) {
				t.Errorf("Families = %d rows, want %d", len(got.Families), len(catalogFixture()))
			}
		})
	}
}

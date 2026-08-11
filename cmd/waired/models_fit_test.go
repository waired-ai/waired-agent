package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// catalogStub serves a minimal /inference/catalog with the given families
// (and an optional status override) so the pull confirmation gate can be
// exercised without a live agent.
func catalogStub(t *testing.T, status int, familiesJSON string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/waired/v1/inference/catalog" {
			http.NotFound(w, r)
			return
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"engine":"ollama","families":` + familiesJSON + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestConfirmModelFitsForPull(t *testing.T) {
	overSpec := `[{"model_id":"qwen3.6-35b-a3b","display_name":"Qwen3.6 35B","fits":false,"deficit_label":"needs 32 GB RAM (have 31 GB)"}]`
	fitsFine := `[{"model_id":"qwen3.5-9b","display_name":"Qwen3.5 9B","fits":true}]`

	// This block inverts what it used to assert, and says so: fits=false
	// was a refusal with no --yes escape (waired-ai/waired#1056,
	// 2026-08-03). The 2026-08-08 owner decision (waired-ai/waired#1067,
	// #583) supersedes that: no surface refuses a model any more. Product
	// contract now pinned: fits=false warns with the shortfall and asks,
	// default No; --yes alone still does not consent (it skips
	// confirmations whose safe answer is yes); --yes --force is the
	// scripted consent; a non-interactive pull without both declines
	// WITHOUT an error — a choice, not a fault.
	t.Run("no memory for it asks; only --yes --force auto-consents", func(t *testing.T) {
		for _, tc := range []struct {
			name             string
			assumeYes, force bool
			wantProceed      bool
		}{
			{"bare decline", false, false, false},
			{"--yes alone declines", true, false, false},
			{"--force alone declines", false, true, false},
			{"--yes --force proceeds", true, true, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				srv := catalogStub(t, http.StatusOK, overSpec)
				var out bytes.Buffer
				proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.6-35b-a3b", tc.assumeYes, tc.force, &out, strings.NewReader(""))
				if err != nil {
					t.Fatalf("err = %v, want nil — a decline is not a fault", err)
				}
				if proceed != tc.wantProceed {
					t.Errorf("proceed = %v, want %v", proceed, tc.wantProceed)
				}
				// The shortfall is what makes the warning actionable.
				if !strings.Contains(out.String(), "needs 32 GB RAM") {
					t.Errorf("output %q does not name the shortfall", out.String())
				}
				if !tc.wantProceed && !strings.Contains(out.String(), "--yes --force") {
					t.Errorf("a decline must name the consent that exists: %q", out.String())
				}
			})
		}
	})

	// The decline line, character for character. The installtest harness
	// greps this exact sentence (IT_PULL_DECLINE_RE in
	// scripts/dev/lib/installtest-enroll.sh) as BOTH a present-assert and
	// an absent-assert, and an absent-assert for wording the product
	// stopped printing passes forever — #178 with the sign flipped. The
	// harness cannot catch its own rename; this is what does, in the same
	// PR. Until the macOS/Windows twins land (#590) this pin stands in for
	// scripts/ci/harness-failure-strings-guard.sh, which checks agreement
	// across three harnesses and so cannot cover a linux-only probe.
	t.Run("the decline line is what the harness greps", func(t *testing.T) {
		srv := catalogStub(t, http.StatusOK, overSpec)
		var out bytes.Buffer
		proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.6-35b-a3b", true, false, &out, strings.NewReader(""))
		if err != nil || proceed {
			t.Fatalf("proceed=%v err=%v, want false/nil — --yes alone declines a fits=false pull", proceed, err)
		}
		const declineLine = "Not downloading. Re-run with --yes --force to download it anyway."
		if !strings.Contains(out.String(), declineLine) {
			t.Errorf("output does not contain the pinned decline line %q:\n%s", declineLine, out.String())
		}
	})

	// The decision seam, all combinations: auto-consent takes BOTH flags,
	// a present human is asked, an absent one declines
	// (waired-ai/waired#1067, 2026-08-08 owner decision).
	t.Run("unfitPullAction table", func(t *testing.T) {
		for _, tc := range []struct {
			assumeYes, force, interactive bool
			want                          pullFitAction
		}{
			{false, false, false, pullDecline},
			{false, false, true, pullAsk},
			{false, true, false, pullDecline},
			{false, true, true, pullAsk},
			{true, false, false, pullDecline},
			{true, false, true, pullAsk},
			{true, true, false, pullProceed},
			{true, true, true, pullProceed},
		} {
			if got := unfitPullAction(tc.assumeYes, tc.force, tc.interactive); got != tc.want {
				t.Errorf("unfitPullAction(yes=%v force=%v tty=%v) = %v, want %v",
					tc.assumeYes, tc.force, tc.interactive, got, tc.want)
			}
		}
	})

	t.Run("not recommended is a confirmation, not a refusal", func(t *testing.T) {
		notRec := `[{"model_id":"qwen3.6-27b","display_name":"Qwen3.6 27B","fits":true,` +
			`"fit":{"runnable":true,"not_recommended":true,"not_recommended_reason":"weights_spill"}}]`
		srv := catalogStub(t, http.StatusOK, notRec)
		var out bytes.Buffer
		proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.6-27b", false, false, &out, strings.NewReader(""))
		if err != nil || !proceed {
			t.Fatalf("proceed=%v err=%v, want true/nil — it runs, it is just not the pick", proceed, err)
		}
		if !strings.Contains(out.String(), "would not choose it here") {
			t.Errorf("the demotion was not explained: %q", out.String())
		}
	})

	t.Run("fitting model is not gated", func(t *testing.T) {
		srv := catalogStub(t, http.StatusOK, fitsFine)
		var out bytes.Buffer
		proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.5-9b", false, false, &out, strings.NewReader(""))
		if err != nil || !proceed {
			t.Fatalf("proceed=%v err=%v, want true/nil", proceed, err)
		}
		if out.Len() != 0 {
			t.Errorf("unexpected output for a fitting model: %q", out.String())
		}
	})

	t.Run("catalog 404 fails open", func(t *testing.T) {
		srv := catalogStub(t, http.StatusNotFound, "")
		var out bytes.Buffer
		proceed, err := confirmModelFitsForPull(srv.URL, "anything", false, false, &out, strings.NewReader(""))
		if err != nil || !proceed {
			t.Fatalf("proceed=%v err=%v, want true/nil (fail-open)", proceed, err)
		}
	})

	t.Run("unmatched model fails open", func(t *testing.T) {
		srv := catalogStub(t, http.StatusOK, overSpec)
		var out bytes.Buffer
		proceed, err := confirmModelFitsForPull(srv.URL, "some-other-model", false, false, &out, strings.NewReader(""))
		if err != nil || !proceed {
			t.Fatalf("proceed=%v err=%v, want true/nil (fail-open)", proceed, err)
		}
	})
}

// Product contract (waired-agent#625): the does-not-fit warning explains
// the smaller of its two figures with this host's own measurement, and
// says nothing when it has no measurement to report.
//
// The sentence exists because "6 GB is allocatable" is true and
// uncheckable on a machine with 16 GB installed. The missing 10 GB is
// the install-time reading #568 takes, and it is mostly resident
// applications rather than the operating system — which is why the copy
// names both.
func TestHostMemoryBreakdown(t *testing.T) {
	for _, tc := range []struct {
		name string
		host catalogDetailHost
		want string
	}{{
		name: "unified host never adds the two figures",
		// A 16 GB Mac: VRAMTotalMB is the synthesized unified budget, so
		// "16 GB RAM + 12 GB graphics memory" would be the same bytes
		// counted twice (waired-ai/waired#1056 decision 1).
		host: catalogDetailHost{
			RAMTotalGB: 16, VRAMTotalMB: 12288, UnifiedMemory: true,
			GPUModel: "Apple M4", OSReservedGB: 10,
		},
		want: "This computer has 16 GB; 10 GB is already in use by the system and other apps.",
	}, {
		name: "discrete host names its card's memory separately",
		host: catalogDetailHost{
			RAMTotalGB: 32, VRAMTotalMB: 8188,
			GPUModel: "NVIDIA GeForce RTX 4070 Laptop GPU", OSReservedGB: 16,
		},
		want: "This computer has 32 GB RAM + 8 GB graphics memory; 16 GB is already in use by the system and other apps.",
	}, {
		name: "cpu-only host",
		host: catalogDetailHost{RAMTotalGB: 8, OSReservedGB: 6},
		want: "This computer has 8 GB; 6 GB is already in use by the system and other apps.",
	}, {
		// The deduction is still the flat floor, so nothing was
		// measured. Printing a constant as though it described this
		// machine is how a figure stops being worth reading.
		name: "unmeasured deduction stays silent",
		host: catalogDetailHost{RAMTotalGB: 32, OSReservedGB: 2},
	}, {
		name: "no RAM reading at all",
		host: catalogDetailHost{OSReservedGB: 10},
	}, {
		name: "older agent that sends no reservation",
		host: catalogDetailHost{RAMTotalGB: 16},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hostMemoryBreakdown(tc.host); got != tc.want {
				t.Errorf("hostMemoryBreakdown = %q, want %q", got, tc.want)
			}
		})
	}
}

// The warning prints the breakdown between the deficit and the
// consequence, and drops the line entirely when there is nothing to
// break down — an older agent's catalog must not produce a half-written
// sentence.
func TestWarnModelDoesNotFitOn_Breakdown(t *testing.T) {
	var withHost bytes.Buffer
	warnModelDoesNotFitOn(&withHost, "Qwen3.5 9B", "needs 11 GB — 6 GB allocatable",
		catalogDetailHost{RAMTotalGB: 16, VRAMTotalMB: 12288, UnifiedMemory: true, OSReservedGB: 10})
	got := withHost.String()
	for _, want := range []string{
		"needs 11 GB — 6 GB allocatable",
		"This computer has 16 GB; 10 GB is already in use",
		"expected to fail after the download completes",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warning %q missing %q", got, want)
		}
	}

	var bare bytes.Buffer
	warnModelDoesNotFit(&bare, "Qwen3.5 9B", "needs 11 GB — 6 GB allocatable")
	if strings.Contains(bare.String(), "already in use") {
		t.Errorf("warning with no host block still claims a measurement: %q", bare.String())
	}
}

// Product contract (waired-agent#632): a row that FITS can still keep
// gigabytes of a coding session off the graphics card, and the surfaces
// say so before the download rather than after it.
//
// The figures are the rc8 Windows host's own (sv-xps15, RTX 4070 Laptop):
// qwen3.5-9b needed 10719 MB to serve the coding window against an
// 8188 MB budget, was marked "fits · recommended", and measured 5 tok/s
// once 6.6 GB had been fetched. qwen3.5-4b needed 7539 MB on the same
// host — it fits on the card, and it is where the benchmark ended up.
func TestContextCacheSpill(t *testing.T) {
	const xps15Budget = 8188
	for _, tc := range []struct {
		name string
		host catalogDetailHost
		fit  *catalogDetailFit
		want int
	}{{
		name: "recommended model that spills (the #632 row)",
		host: catalogDetailHost{RAMTotalGB: 32, GPUBudgetMB: xps15Budget},
		fit:  &catalogDetailFit{Runnable: true, RequiredWindowResidentMB: 10719},
		want: 2531,
	}, {
		name: "the model the benchmark switched to",
		host: catalogDetailHost{RAMTotalGB: 32, GPUBudgetMB: xps15Budget},
		fit:  &catalogDetailFit{Runnable: true, RequiredWindowResidentMB: 7539},
	}, {
		// No card is not a small card. "0 GB spills" would be true and
		// useless, and "it all runs from RAM" is the CPU-only story
		// rather than this sentence's job.
		name: "cpu-only host says nothing",
		host: catalogDetailHost{RAMTotalGB: 32},
		fit:  &catalogDetailFit{Runnable: true, RequiredWindowResidentMB: 10719},
	}, {
		name: "variant the projection could not price",
		host: catalogDetailHost{RAMTotalGB: 32, GPUBudgetMB: xps15Budget},
		fit:  &catalogDetailFit{Runnable: true},
	}, {
		name: "row with no projection at all",
		host: catalogDetailHost{RAMTotalGB: 32, GPUBudgetMB: xps15Budget},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := contextCacheSpillMB(tc.host, tc.fit); got != tc.want {
				t.Errorf("contextCacheSpillMB = %d, want %d", got, tc.want)
			}
			note := contextCacheSpillNote(tc.host, tc.fit)
			if (note != "") != (tc.want > 0) {
				t.Errorf("note = %q for spill %d", note, tc.want)
			}
			if tc.want > 0 && !strings.Contains(note, "2.5 GB") {
				t.Errorf("note %q does not quote the shortfall as 2.5 GB", note)
			}
		})
	}
}

// The note is stated, never asked. Excluding a model on a PREDICTED rate
// is what docs/decisions/20260804/1937-… decision 4 removed; speed
// returns as a recommendation input when it is MEASURED
// (waired-agent#466). A second default-No prompt here would be that
// exclusion by the back door, so `models pull` must still proceed
// without asking.
func TestConfirmModelFitsForPull_SpillIsStatedNotAsked(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"engine":"ollama",
			"host":{"ram_total_gb":32,"vram_total_mb":8188,"gpu_budget_mb":8188,"os_reserved_gb":16},
			"families":[{"model_id":"qwen3.5-9b","display_name":"Qwen3.5 9B","fits":true,
			"recommended_pick":true,
			"fit":{"runnable":true,"required_window_resident_mb":10719}}]}`))
	}))
	t.Cleanup(srv.Close)

	var out bytes.Buffer
	proceed, err := confirmModelFitsForPull(srv.URL, "qwen3.5-9b", false, false, &out, strings.NewReader(""))
	if err != nil {
		t.Fatalf("confirmModelFitsForPull: %v", err)
	}
	if !proceed {
		t.Error("a fitting, recommended model was blocked by the spill note")
	}
	got := out.String()
	if !strings.Contains(got, "2.5 GB") || !strings.Contains(got, "system RAM") {
		t.Errorf("pull output does not state the shortfall: %q", got)
	}
	if strings.Contains(got, "[y/N]") || strings.Contains(got, "[Y/n]") {
		t.Errorf("pull output asks a question about a recommended model: %q", got)
	}
}

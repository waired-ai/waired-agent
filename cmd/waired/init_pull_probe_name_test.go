package main

import (
	"strings"
	"testing"
	"time"

	"github.com/waired-ai/waired-agent/internal/management"
	"github.com/waired-ai/waired-agent/proto/hostfit"
)

// PRODUCT CONTRACT (waired-agent#1299): the terminal never draws the
// host-cutoff probe's transfer under another model's name.
//
// waitHasTarget has excluded the probe since waired-agent#736, but the
// exclusion lived only there — so the wait correctly knew it had no
// target while every line on the screen said "the model" over the probe's
// 1.0 GB. Reproduced on all four hosts of the rc6 review.
func TestDownloadLabel_NamesTheProbeAsItself(t *testing.T) {
	const probe = hostfit.HostCutoffProbeModelID
	for _, tc := range []struct {
		name string
		dl   management.ModelDownload
		want string
		out  string
	}{
		{"the probe is named as the small model", management.ModelDownload{Model: probe}, "", hostCutoffProbeLabel},
		{"a chosen model is named by its id", management.ModelDownload{Model: "qwen3.5-27b"}, "", "qwen3.5-27b"},
		{"a wizard target outranks the download", management.ModelDownload{Model: probe}, "qwen3.8-27b", "qwen3.8-27b"},
		{"a download with no model falls back", management.ModelDownload{}, "", "the model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := downloadLabel(tc.dl, tc.want); got != tc.out {
				t.Errorf("downloadLabel = %q, want %q", got, tc.out)
			}
		})
	}
}

// PRODUCT CONTRACT: when a chosen model and the probe are both in flight,
// the bar belongs to the chosen one.
//
// Downloads is built by ranging a map in the daemon, so Downloads[0] is
// whichever the runtime happened to hand back — the reason the old
// fallback could count the probe's bytes under the chosen model's name.
func TestActiveDownload_PrefersTheChosenModelOverTheProbe(t *testing.T) {
	const probe = hostfit.HostCutoffProbeModelID
	st := management.InferenceStatus{
		Models: management.ModelsSnapshot{Downloads: []management.ModelDownload{
			{Model: probe, CompletedBytes: 900, TotalBytes: 1000},
			{Model: "qwen3.5-27b", CompletedBytes: 10, TotalBytes: 17_000},
		}},
	}
	got, ok := activeDownload(st)
	if !ok {
		t.Fatal("activeDownload ok=false, want true")
	}
	if got.Model != "qwen3.5-27b" {
		t.Errorf("activeDownload = %q, want the chosen model", got.Model)
	}

	// With only the probe in flight it is still drawn — a bar beats a
	// silent terminal — just under its own name.
	only := management.InferenceStatus{
		Models: management.ModelsSnapshot{Downloads: []management.ModelDownload{
			{Model: probe, CompletedBytes: 900, TotalBytes: 1000},
		}},
	}
	got, ok = activeDownload(only)
	if !ok || got.Model != probe {
		t.Errorf("activeDownload with only the probe = %q, ok=%v; want the probe drawn", got.Model, ok)
	}
}

// PRODUCT CONTRACT: the step notes before the bar name the probe too.
// "Preparing to download the model..." over a 1.0 GB measurement is the
// line the rc6 transcripts open with, twice.
func TestActiveSubjectName_TellsTheProbeFromAChosenModel(t *testing.T) {
	const probe = hostfit.HostCutoffProbeModelID
	for _, tc := range []struct {
		name string
		st   management.InferenceStatus
		out  string
	}{{
		name: "only the probe is in flight",
		st: management.InferenceStatus{
			Models: management.ModelsSnapshot{Downloading: []string{probe}},
		},
		out: hostCutoffProbeLabel,
	}, {
		name: "the probe is active and downloading",
		st: management.InferenceStatus{
			Active: activeSel(probe),
			Models: management.ModelsSnapshot{Downloading: []string{probe}},
		},
		out: hostCutoffProbeLabel,
	}, {
		name: "a chosen model beside the probe wins",
		st: management.InferenceStatus{
			Active: activeSel(probe),
			Models: management.ModelsSnapshot{Downloading: []string{probe, "qwen3.5-27b"}},
		},
		out: "qwen3.5-27b",
	}, {
		name: "an active chosen model wins",
		st: management.InferenceStatus{
			Active: activeSel("qwen3.5-27b"),
		},
		out: "qwen3.5-27b",
	}, {
		name: "the probe resident, nothing in flight, keeps its id",
		st: management.InferenceStatus{
			Active: activeSel(probe),
			Models: management.ModelsSnapshot{Ready: []string{probe}},
		},
		out: probe,
	}, {
		name: "nothing at all",
		st:   management.InferenceStatus{SubsystemState: "awaiting_model"},
		out:  "the model",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			if got := activeSubjectName(tc.st); got != tc.out {
				t.Errorf("activeSubjectName = %q, want %q", got, tc.out)
			}
		})
	}
}

// PRODUCT CONTRACT: waitModelName stays an id, because the failure lines
// interpolate it into a copy-pasteable `waired models pull <id>`. The
// probe's prose label would be three shell arguments and no model.
func TestWaitModelName_StaysAnIdWhileTheProbeIsInFlight(t *testing.T) {
	const probe = hostfit.HostCutoffProbeModelID
	st := management.InferenceStatus{
		Models: management.ModelsSnapshot{
			Downloading: []string{probe},
			Downloads:   []management.ModelDownload{{Model: probe, TotalBytes: 1000}},
		},
	}
	if got := waitModelName(st, ""); got != "the model" {
		t.Errorf("waitModelName = %q, want the generic label rather than the probe's prose name", got)
	}
	if strings.Contains(waitModelName(st, ""), " ") && waitModelName(st, "") != "the model" {
		t.Errorf("waitModelName = %q, want something a shell can take as one argument", waitModelName(st, ""))
	}
}

// End to end through the wait, with the probe's transfer actually in
// flight: the table above pins the discriminator, this pins that the
// screen consults it. Both are needed — waired-agent#736 shipped with a
// passing table and a passing end-to-end test because neither drove a
// download.
func TestWaitForBundledModel_DrawsTheProbeUnderItsOwnName(t *testing.T) {
	setBenchTiming(t, time.Millisecond, 5*time.Second, time.Minute)
	const mb = 1 << 20
	probe := hostfit.HostCutoffProbeModelID
	stub := &pullStub{seq: []management.InferenceStatus{
		downloadingSnap(probe, 200*mb, 1000*mb),
		downloadingSnap(probe, 900*mb, 1000*mb),
		{
			SubsystemState: "awaiting_model",
			Models:         management.ModelsSnapshot{Ready: []string{probe}},
		},
	}}
	srv := stub.server()
	defer srv.Close()

	var out strings.Builder
	waitForBundledModel(srv.URL, &out, false, 60*time.Millisecond, false, nil, nil, nil)

	got := out.String()
	if !strings.Contains(got, "the small model used to time this computer") {
		t.Errorf("output %q never says what the 1.0 GB transfer is", got)
	}
	if strings.Contains(got, "Downloading the model:") {
		t.Errorf("output %q still draws the probe as the model", got)
	}
	if strings.Contains(got, "several GB") {
		t.Errorf("output %q calls a 1.0 GB measurement several GB", got)
	}
}

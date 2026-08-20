package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/management"
	infruntime "github.com/waired-ai/waired-agent/internal/runtime"
)

// --- vLLM progress reporting (waired-agent#255) ---
//
// The ollama half of this lives in ollama_progress_line_test.go; these
// are its counterparts for the engine whose installer is staged rather
// than byte-denominated end to end.

func TestVLLMProgressStage(t *testing.T) {
	// pip-install is the only stage that transfers anything — ~4 GB of
	// it. Getting this wrong shows a byte bar on a step that has no bytes
	// (or hides the one that does), which is the ollama defect this
	// mapping was copied from.
	for stage, want := range map[infruntime.InstallStage]string{
		infruntime.StagePipInstall: management.SetupStepEngineDownload,
		infruntime.StageResolveUV:  management.SetupStepEngineInstall,
		infruntime.StageCreateVenv: management.SetupStepEngineInstall,
		infruntime.StageToolchain:  management.SetupStepEngineInstall,
		infruntime.StageVerify:     management.SetupStepEngineInstall,
		infruntime.StageActivate:   management.SetupStepEngineInstall,
		"":                         management.SetupStepEngineInstall,
	} {
		if got := vllmProgressStage(stage); got != want {
			t.Errorf("vllmProgressStage(%q) = %q, want %q", stage, got, want)
		}
	}
}

func TestVLLMProgressSinkMapsInstallerStages(t *testing.T) {
	shrinkSetupTimers(t)
	shrinkProgressThrottle(t)
	d := &fakeSetupDaemon{}
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()
	s.Installing("vllm")

	sink := newVLLMProgressSink(s, "vllm")
	if sink == nil {
		t.Fatal("newVLLMProgressSink returned nil for a live session")
	}

	// pip-install before uv has announced anything: the stage is running
	// but there is no size yet, and opening the download row here would
	// draw a bar the very next event has to resize. The ollama sink
	// refuses the equivalent stage-opening event for the same reason.
	sink(infruntime.InstallProgress{Stage: infruntime.StagePipInstall, Message: "installing vllm..."})
	if got := progressReports(d.noted(), management.SetupStepEngineDownload); len(got) != 0 {
		t.Fatalf("byte-less pip-install opened the download row: %+v", got)
	}

	// Once uv announces, the figures reported are the installer's own.
	sink(infruntime.InstallProgress{
		Stage:          infruntime.StagePipInstall,
		CompletedBytes: 1 << 30,
		TotalBytes:     4 << 30,
		BytesPerSec:    50 << 20,
	})
	got := progressReports(d.noted(), management.SetupStepEngineDownload)
	if len(got) == 0 {
		t.Fatal("no engine_download report after uv announced a download")
	}
	last := got[len(got)-1]
	if last.CompletedBytes != 1<<30 || last.TotalBytes != 4<<30 || last.RateBps != 50<<20 {
		t.Errorf("download report = %d/%d @%d, want %d/%d @%d",
			last.CompletedBytes, last.TotalBytes, last.RateBps, int64(1<<30), int64(4<<30), int64(50<<20))
	}
	if last.Engine != "vllm" {
		t.Errorf("engine = %q, want vllm", last.Engine)
	}

	// verify belongs to the install proper, which §7 denominates
	// indeterminate: no bytes may ride it.
	sink(infruntime.InstallProgress{Stage: infruntime.StageVerify, Message: "verifying..."})
	install := progressReports(d.noted(), management.SetupStepEngineInstall)
	if len(install) == 0 {
		t.Fatal("verify did not report on the engine_install row")
	}
	for _, r := range install {
		if r.CompletedBytes != 0 || r.TotalBytes != 0 {
			t.Errorf("engine_install carries bytes %d/%d — that row is indeterminate",
				r.CompletedBytes, r.TotalBytes)
		}
	}
}

func TestVLLMProgressSinkInertWhenUnsupported(t *testing.T) {
	// A host with no daemon routes gets nil, which is exactly what
	// "render to stdout only" already looks like to the installer.
	shrinkSetupTimers(t)
	d := &fakeSetupDaemon{notFound: true}
	srv := d.server(t)

	s := attachSetupExecutor(srv.URL, true)
	defer s.Release()

	if sink := newVLLMProgressSink(s, "vllm"); sink != nil {
		t.Error("want nil for an inert session")
	}
}

func TestTeeProgressIsGenericOverTheEventType(t *testing.T) {
	// teeOllamaProgress is this function at the ollama type; the vLLM
	// path needs the same fan-out for a different struct.
	t.Run("nil-only collapses to nil", func(t *testing.T) {
		if teeProgress[infruntime.InstallProgress](nil, nil) != nil {
			t.Error("want nil so the installer takes its no-callback path")
		}
	})
	t.Run("both peers see every event", func(t *testing.T) {
		var a, b int
		fan := teeProgress(
			func(infruntime.InstallProgress) { a++ },
			nil,
			func(infruntime.InstallProgress) { b++ },
		)
		fan(infruntime.InstallProgress{Stage: infruntime.StagePipInstall})
		fan(infruntime.InstallProgress{Stage: infruntime.StageVerify})
		if a != 2 || b != 2 {
			t.Errorf("a=%d b=%d, want 2 and 2", a, b)
		}
	})
}

func TestRenderVLLMInstallProgress(t *testing.T) {
	// The terminal half. Before #255 the operator watched a silent ~4 GB
	// download here, because the percentage this used to print is never
	// populated when uv's output is piped. The byte grammar matches the
	// ollama renderer's so one terminal reads the same for both engines.
	cases := []struct {
		name string
		in   infruntime.InstallProgress
		want string
	}{
		{
			name: "bytes and rate",
			in: infruntime.InstallProgress{
				Stage: infruntime.StagePipInstall, Step: 3, Total: 5,
				Percent:        -1,
				CompletedBytes: 2_100_000_000,
				TotalBytes:     4_209_403_494,
				BytesPerSec:    24_100_000,
				Message:        "Downloading torch (506.1MiB)",
			},
			want: "[3/5 pip-install]  49%  2.1 GB / 4.2 GB (24.1 MB/s) Downloading torch (506.1MiB)\n",
		},
		{
			name: "bytes without a rate sample yet",
			in: infruntime.InstallProgress{
				Stage: infruntime.StagePipInstall, Step: 3, Total: 5,
				Percent:    -1,
				TotalBytes: 4_209_403_494,
				Message:    "Downloading torch (506.1MiB)",
			},
			want: "[3/5 pip-install]   0%  0 B / 4.2 GB Downloading torch (506.1MiB)\n",
		},
		{
			name: "no bytes, no percent — the other four stages",
			in: infruntime.InstallProgress{
				Stage: infruntime.StageVerify, Step: 4, Total: 5,
				Percent: -1,
				Message: "verifying...",
			},
			want: "[4/5 verify] verifying...\n",
		},
		{
			name: "percent still renders when a tool line carries one",
			in: infruntime.InstallProgress{
				Stage: infruntime.StageCreateVenv, Step: 2, Total: 5,
				Percent: 47,
				Message: "something 47%",
			},
			want: "[2/5 create-venv] 47% something 47%\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderVLLMInstallProgress(&buf)(c.in)
			if got := buf.String(); got != c.want {
				t.Errorf("got  %q\nwant %q", got, c.want)
			}
		})
	}
}

// TestVLLMInstallCoreTeesToBothConsumers pins the peer rule: attaching
// the daemon sink must not cost the operator their own output, and a
// host with no daemon routes must keep the terminal rendering it had.
func TestVLLMInstallCoreTeesToBothConsumers(t *testing.T) {
	prev := vllmInstall
	t.Cleanup(func() { vllmInstall = prev })
	vllmInstall = func(_ context.Context, _ string, _ bool, onProgress func(infruntime.InstallProgress)) (infruntime.InstallResult, error) {
		onProgress(infruntime.InstallProgress{
			Stage: infruntime.StagePipInstall, Step: 3, Total: 5, Percent: -1,
			CompletedBytes: 1 << 30, TotalBytes: 4 << 30, Message: "Downloading torch (506.1MiB)",
		})
		return infruntime.InstallResult{}, nil
	}

	var seen []infruntime.InstallProgress
	out := captureStdout(t, func() {
		if _, err := vllmInstallCore(context.Background(), t.TempDir(), true, func(p infruntime.InstallProgress) {
			seen = append(seen, p)
		}); err != nil {
			t.Fatalf("vllmInstallCore: %v", err)
		}
	})

	if len(seen) != 1 {
		t.Errorf("sink saw %d events, want 1", len(seen))
	}
	if !strings.Contains(out, "pip-install") || !strings.Contains(out, "1.1 GB / 4.3 GB") {
		t.Errorf("terminal output lost its line: %q", out)
	}
}

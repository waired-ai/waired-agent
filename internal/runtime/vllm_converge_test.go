package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// want is the pin set under test, spelled out so a case can vary one
// member without restating the rest.
func wantPins() VLLMPinSet {
	return VLLMPinSet{
		VLLM:         "0.24.0",
		HFTransfer:   "0.1.9",
		Transformers: "transformers>=5.5.3,<6.0",
		Python:       "3.12",
	}
}

// The policy, stated once and table-tested, because three call sites act
// on it: `waired runtimes upgrade vllm`, the daemon's start-up backstop,
// and install.sh through the first. #843.
func TestDecideVLLMConverge(t *testing.T) {
	atPin := wantPins()

	cases := []struct {
		name        string
		facts       VLLMConvergeFacts
		wantInstall bool
		wantBlocked bool
		wantWhy     string // substring
	}{
		{
			// #138: `waired init` is the only thing that decides this
			// computer runs models, and a venv is a ~6 GB answer.
			name:    "absent stays absent",
			facts:   VLLMConvergeFacts{Installed: false, Want: atPin},
			wantWhy: "no vLLM venv",
		},
		{
			name: "at the pin set",
			facts: VLLMConvergeFacts{
				Installed: true, Version: "0.24.0",
				Recorded: atPin, HasRecord: true, Want: atPin,
			},
			wantWhy: "already at the pin set",
		},
		{
			// The case every host that installed before #843 is in. Its
			// vLLM version matches; nothing is known about the wheels
			// beside it, and "unknown" must not cost ~6 GB.
			name: "no record, version matches",
			facts: VLLMConvergeFacts{
				Installed: true, Version: "0.24.0",
				HasRecord: false, Want: atPin,
			},
			wantWhy: "predates the pin record",
		},
		{
			name: "older venv",
			facts: VLLMConvergeFacts{
				Installed: true, Version: "0.20.0",
				Recorded: VLLMPinSet{VLLM: "0.20.0"}, HasRecord: true, Want: atPin,
			},
			wantInstall: true,
			wantWhy:     "venv is 0.20.0, pin is 0.24.0",
		},
		{
			// Exact match, not a floor: a pin moves backwards when a
			// release is withdrawn, and this build's parser table and
			// serve flags came out of the pinned release, so a NEWER
			// venv is as untested as an older one.
			name: "newer venv converges down",
			facts: VLLMConvergeFacts{
				Installed: true, Version: "0.25.0",
				Recorded: VLLMPinSet{VLLM: "0.25.0"}, HasRecord: true, Want: atPin,
			},
			wantInstall: true,
			wantWhy:     "venv is 0.25.0, pin is 0.24.0",
		},
		{
			// The reason this compares strings and not dotted cores.
			// PyPI post releases differ from their base in exactly the
			// part internal/version.Compare discards, and a pin moved to
			// one is a pin moved to fix something.
			name: "a post release is a different release",
			facts: VLLMConvergeFacts{
				Installed: true, Version: "0.24.0.post1",
				Recorded: VLLMPinSet{VLLM: "0.24.0.post1"}, HasRecord: true, Want: atPin,
			},
			wantInstall: true,
			wantWhy:     "0.24.0.post1",
		},
		{
			// The companion pins are why the record exists at all: the
			// version DIRECTORY is named after the vLLM release, so a
			// host in this state looks up to date by name.
			name: "hf_transfer moved on its own",
			facts: VLLMConvergeFacts{
				Installed: true, Version: "0.24.0", HasRecord: true, Want: atPin,
				Recorded: func() VLLMPinSet { p := atPin; p.HFTransfer = "0.1.8"; return p }(),
			},
			wantInstall: true,
			wantWhy:     "hf_transfer is 0.1.8",
		},
		{
			name: "transformers constraint moved on its own",
			facts: VLLMConvergeFacts{
				Installed: true, Version: "0.24.0", HasRecord: true, Want: atPin,
				Recorded: func() VLLMPinSet { p := atPin; p.Transformers = "transformers<5.0"; return p }(),
			},
			wantInstall: true,
			wantWhy:     "transformers constraint",
		},
		{
			name: "interpreter moved on its own",
			facts: VLLMConvergeFacts{
				Installed: true, Version: "0.24.0", HasRecord: true, Want: atPin,
				Recorded: func() VLLMPinSet { p := atPin; p.Python = "3.11"; return p }(),
			},
			wantInstall: true,
			wantWhy:     "Python 3.11",
		},
		{
			name: "a venv that cannot name its version is rebuilt",
			facts: VLLMConvergeFacts{
				Installed: true, Version: "", HasRecord: false, Want: atPin,
			},
			wantInstall: true,
			wantWhy:     "does not name a version",
		},
		{
			// Blocked is not "nothing to do". The rebuild is needed and
			// nothing will change until somebody frees space, so the
			// callers warn rather than log a debug line.
			name: "needed but the disk is too full",
			facts: VLLMConvergeFacts{
				Installed: true, Version: "0.20.0", HasRecord: true, Want: atPin,
				Recorded: VLLMPinSet{VLLM: "0.20.0"}, FreeBytes: 2 << 30,
			},
			wantBlocked: true,
			wantWhy:     "only 2.0 GB is free",
		},
		{
			// A statfs that failed is not evidence of a full disk, and
			// the install has its own ENOSPC.
			name: "unknown free space does not block",
			facts: VLLMConvergeFacts{
				Installed: true, Version: "0.20.0", HasRecord: true, Want: atPin,
				Recorded: VLLMPinSet{VLLM: "0.20.0"}, FreeBytes: 0,
			},
			wantInstall: true,
		},
		{
			// Disk is only ever consulted for a converge that would
			// otherwise run: a host with no venv is not "blocked".
			name: "absent is not blocked by a full disk",
			facts: VLLMConvergeFacts{
				Installed: false, Want: atPin, FreeBytes: 1 << 20,
			},
			wantWhy: "no vLLM venv",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := DecideVLLMConverge(c.facts)
			if got.Install != c.wantInstall {
				t.Errorf("Install = %v, want %v (reason: %s)", got.Install, c.wantInstall, got.Reason)
			}
			if got.Blocked != c.wantBlocked {
				t.Errorf("Blocked = %v, want %v (reason: %s)", got.Blocked, c.wantBlocked, got.Reason)
			}
			if got.Install && got.Blocked {
				t.Errorf("a blocked converge must not also install (reason: %s)", got.Reason)
			}
			if got.Reason == "" {
				t.Error("every decision carries a reason; it is printed at the end of an update")
			}
			if c.wantWhy != "" && !strings.Contains(got.Reason, c.wantWhy) {
				t.Errorf("Reason = %q, want it to contain %q", got.Reason, c.wantWhy)
			}
		})
	}
}

// A blocked decision names both numbers, because "not enough disk" that
// does not say how much is short is not actionable.
func TestDecideVLLMConverge_BlockedReasonNamesBothSides(t *testing.T) {
	got := DecideVLLMConverge(VLLMConvergeFacts{
		Installed: true, Version: "0.20.0", HasRecord: true,
		Recorded: VLLMPinSet{VLLM: "0.20.0"}, Want: wantPins(), FreeBytes: 3 << 30,
	})
	for _, want := range []string{"3.0 GB", "8 GB", "0.20.0", "0.24.0"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("Reason = %q, want it to contain %q", got.Reason, want)
		}
	}
}

func TestConvergeVLLM_InstallsOnlyWhenDecided(t *testing.T) {
	cases := []struct {
		name        string
		installed   bool
		version     string
		wantInstall bool
	}{
		{name: "absent", installed: false, wantInstall: false},
		{name: "current", installed: true, version: VLLMPinnedVersion, wantInstall: false},
		{name: "stale", installed: true, version: "0.20.0", wantInstall: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			installed, pruned, pinsRead := false, false, false
			got, err := ConvergeVLLM(context.Background(), VLLMConvergeDeps{
				Active: func() (string, bool) { return c.version, c.installed },
				Pins: func() (VLLMPinSet, bool) {
					pinsRead = true
					return WantedVLLMPins(), true
				},
				FreeBytes: func() int64 { return 500 << 30 },
				Install:   func(context.Context) error { installed = true; return nil },
				Prune:     func() ([]string, error) { pruned = true; return nil, nil },
			})
			if err != nil {
				t.Fatalf("ConvergeVLLM: %v", err)
			}
			if installed != c.wantInstall {
				t.Errorf("installed = %v, want %v (decision: %+v)", installed, c.wantInstall, got)
			}
			if got.Install != c.wantInstall {
				t.Errorf("decision.Install = %v, want %v", got.Install, c.wantInstall)
			}
			if pruned != c.wantInstall {
				t.Errorf("pruned = %v, want %v: superseded venvs are reclaimed only after an install", pruned, c.wantInstall)
			}
			// A host with no venv must cost nothing to check — no
			// record read, no statfs. This is every macOS and Windows
			// host, on every agent start.
			if pinsRead != c.installed {
				t.Errorf("pins read = %v, want %v", pinsRead, c.installed)
			}
		})
	}
}

// A failed install surfaces as an error AND keeps the decision, so a
// caller can say what it was trying to do when it failed — and it must
// not prune, because the venv the host is still serving from is the one
// that would go.
func TestConvergeVLLM_InstallFailureIsReportedAndPrunesNothing(t *testing.T) {
	boom := errors.New("network down")
	pruned := false
	got, err := ConvergeVLLM(context.Background(), VLLMConvergeDeps{
		Active:  func() (string, bool) { return "0.20.0", true },
		Pins:    func() (VLLMPinSet, bool) { return VLLMPinSet{VLLM: "0.20.0"}, true },
		Install: func(context.Context) error { return boom },
		Prune:   func() ([]string, error) { pruned = true; return nil, nil },
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want it to wrap %v", err, boom)
	}
	if !got.Install {
		t.Error("the decision to install is lost on failure; the caller cannot explain itself")
	}
	if pruned {
		t.Error("pruned after a failed install: that removes the venv the host is still serving from")
	}
}

// Failing to reclaim ~6 GB is not a failed converge. The engine is at
// the pin either way, so the prune outcome rides alongside rather than
// turning into the error.
func TestConvergeVLLM_PruneFailureDoesNotFailTheConverge(t *testing.T) {
	boom := errors.New("permission denied")
	got, err := ConvergeVLLM(context.Background(), VLLMConvergeDeps{
		Active:  func() (string, bool) { return "0.20.0", true },
		Pins:    func() (VLLMPinSet, bool) { return VLLMPinSet{VLLM: "0.20.0"}, true },
		Install: func(context.Context) error { return nil },
		Prune:   func() ([]string, error) { return []string{"0.20.0"}, boom },
	})
	if err != nil {
		t.Fatalf("ConvergeVLLM: %v", err)
	}
	if !errors.Is(got.PruneErr, boom) {
		t.Errorf("PruneErr = %v, want it to wrap %v", got.PruneErr, boom)
	}
	if len(got.Pruned) != 1 || got.Pruned[0] != "0.20.0" {
		t.Errorf("Pruned = %v, want the versions it did remove to still be reported", got.Pruned)
	}
}

// WantedVLLMPins is the one place the set is assembled; if a member is
// ever added to VLLMPinSet and not wired here it would compare equal to
// "" for ever and no host would converge on it.
func TestWantedVLLMPins_IsFullyPopulated(t *testing.T) {
	got := WantedVLLMPins()
	for name, v := range map[string]string{
		"VLLM":         got.VLLM,
		"HFTransfer":   got.HFTransfer,
		"Transformers": got.Transformers,
		"Python":       got.Python,
	} {
		if v == "" {
			t.Errorf("WantedVLLMPins().%s is empty: a pin nobody sets is a pin nobody converges on", name)
		}
	}
}

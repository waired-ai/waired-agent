package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func settingsWith(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	if body != "" {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func envOf(t *testing.T, path string) map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s is not JSON: %v", path, err)
	}
	return m.Env
}

func TestDetectSubagentPlacement(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want SubagentPlacement
	}{
		{"no file", "", SubagentFollow},
		{"no env block", `{"statusLine":{"type":"command","command":"x"}}`, SubagentFollow},
		{"env without the key", `{"env":{"FOO":"bar"}}`, SubagentFollow},
		{"ours", `{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"waired"}}`, SubagentWaired},
		// The retired label counts as ours: a machine that enabled before
		// waired-agent#1186 has it, and reporting it as somebody else's would
		// stop waired from clearing it.
		{"the retired label", `{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"waired/subagent"}}`, SubagentWaired},
		{"an operator's own choice", `{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"claude-haiku-4-5"}}`, SubagentForeign},
		{"not JSON", `{`, SubagentUnreadable},
		{"env is not an object", `{"env":"nope"}`, SubagentUnreadable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := DetectSubagentPlacement(settingsWith(t, tc.body)); got != tc.want {
				t.Errorf("DetectSubagentPlacement(%s) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestSetSubagentPlacementWritesBothVariables is the measured half of this
// switch. Without CLAUDE_CODE_SUBAGENT_MODEL_FORCE an agent definition's own
// `model:` wins, so "subagents on Waired" would quietly not apply to the
// agents most likely to have been given a model on purpose — measured against
// Claude Code 2.1.261 on 2026-09-06 with a definition pinning claude-opus-4-8
// (see docs/knowledges/20260906/0350).
func TestSetSubagentPlacementWritesBothVariables(t *testing.T) {
	p := settingsWith(t, `{"statusLine":{"type":"command","command":"keep me"}}`)

	changed, err := SetSubagentPlacement(p, SubagentWaired, DirectiveModelAny)
	if err != nil || !changed {
		t.Fatalf("SetSubagentPlacement = (%v, %v), want (true, nil)", changed, err)
	}
	env := envOf(t, p)
	if env["CLAUDE_CODE_SUBAGENT_MODEL"] != DirectiveModelAny {
		t.Errorf("CLAUDE_CODE_SUBAGENT_MODEL = %q, want %q", env["CLAUDE_CODE_SUBAGENT_MODEL"], DirectiveModelAny)
	}
	if env["CLAUDE_CODE_SUBAGENT_MODEL_FORCE"] != "1" {
		t.Errorf("CLAUDE_CODE_SUBAGENT_MODEL_FORCE = %q, want \"1\" — without it an "+
			"agent definition's own model wins", env["CLAUDE_CODE_SUBAGENT_MODEL_FORCE"])
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "keep me") {
		t.Errorf("the operator's own key was dropped: %s", b)
	}

	// Writing the same thing twice changes nothing.
	if changed, err := SetSubagentPlacement(p, SubagentWaired, DirectiveModelAny); err != nil || changed {
		t.Errorf("second write = (%v, %v), want (false, nil)", changed, err)
	}

	// Back to follow takes BOTH away: the force flag does nothing on its own
	// and waired is what put it there, so leaving it would be litter that
	// changes behaviour the moment anyone sets a subagent model again.
	if changed, err := SetSubagentPlacement(p, SubagentFollow, DirectiveModelAny); err != nil || !changed {
		t.Fatalf("back to follow = (%v, %v), want (true, nil)", changed, err)
	}
	env = envOf(t, p)
	for _, k := range []string{"CLAUDE_CODE_SUBAGENT_MODEL", "CLAUDE_CODE_SUBAGENT_MODEL_FORCE"} {
		if v, bad := env[k]; bad {
			t.Errorf("%s = %q, want absent", k, v)
		}
	}
}

func TestSetSubagentPlacementLeavesForeignValuesAlone(t *testing.T) {
	p := settingsWith(t, `{"env":{"CLAUDE_CODE_SUBAGENT_MODEL":"claude-haiku-4-5"}}`)
	if _, err := SetSubagentPlacement(p, SubagentWaired, DirectiveModelAny); err == nil {
		t.Fatal("SetSubagentPlacement replaced a subagent model waired did not write")
	}
	// Including on the way out: `waired claude disable` must not delete a
	// choice somebody else made.
	if changed, err := SetSubagentPlacement(p, SubagentFollow, DirectiveModelAny); err == nil || changed {
		t.Errorf("follow = (%v, %v), want a refusal", changed, err)
	}
	if got := envOf(t, p)["CLAUDE_CODE_SUBAGENT_MODEL"]; got != "claude-haiku-4-5" {
		t.Errorf("value = %q, want it untouched", got)
	}
}

func TestSetSubagentPlacementRefusesUnreadableSettings(t *testing.T) {
	p := settingsWith(t, `{`)
	if _, err := SetSubagentPlacement(p, SubagentWaired, DirectiveModelAny); err == nil {
		t.Fatal("SetSubagentPlacement rewrote a file it could not read")
	}
	b, _ := os.ReadFile(p)
	if string(b) != `{` {
		t.Errorf("the file was rewritten anyway: %s", b)
	}
}

// An empty env block is removed rather than left as `"env":{}`: the next
// reader should see the same thing a host that never ran this sees.
func TestSetSubagentPlacementLeavesNoEmptyEnv(t *testing.T) {
	p := settingsWith(t, "")
	if _, err := SetSubagentPlacement(p, SubagentWaired, DirectiveModelAny); err != nil {
		t.Fatal(err)
	}
	if _, err := SetSubagentPlacement(p, SubagentFollow, DirectiveModelAny); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Contains(string(b), "env") {
		t.Errorf("an empty env block was left behind: %s", b)
	}
}

package main

import (
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/internal/runtime/state"
)

// waired-agent#789: `waired claude route anthropic --subagents waired`
// followed by `waired claude route auto` left subagents pinned to waired
// — on all three OSes. The command reads as "back to the defaults" and
// only an explicit --subagents same undid it, so subagent traffic kept
// going somewhere the user had stopped asking for.
//
// The rule now: the positional argument is all of Claude Code, the flags
// are one class each. This table is that sentence.

func routeOf(t *testing.T, p *state.ClaudeRouteClass) string {
	t.Helper()
	if p == nil {
		return "<unchanged>"
	}
	return string(*p)
}

func TestPlanClaudeRouteChange(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      claudeRouteArgs
		wantShow  bool
		wantMain  string
		wantSub   string
		wantClear bool
	}{
		{
			name:     "nothing asked for",
			args:     claudeRouteArgs{},
			wantShow: true,
		},
		{
			// The regression. Subagents come back to the main conversation
			// because the user moved all of Claude Code.
			name:     "a bare route sets both",
			args:     claudeRouteArgs{positional: []string{"auto"}},
			wantMain: "auto", wantSub: "same", wantClear: true,
		},
		{
			name:     "a bare route to waired sets both",
			args:     claudeRouteArgs{positional: []string{"waired"}},
			wantMain: "waired", wantSub: "same", wantClear: true,
		},
		{
			// "local" is the back-compat spelling of "waired"; it must
			// keep resetting subagents like any other positional.
			name:     "the local alias behaves like waired",
			args:     claudeRouteArgs{positional: []string{"local"}},
			wantMain: "waired", wantSub: "same", wantClear: true,
		},
		{
			name:     "--main leaves subagents alone",
			args:     claudeRouteArgs{main: "anthropic", mainSet: true},
			wantMain: "anthropic", wantSub: "<unchanged>",
		},
		{
			name:     "--sub leaves the main conversation alone",
			args:     claudeRouteArgs{sub: "waired", subSet: true},
			wantMain: "<unchanged>", wantSub: "waired",
		},
		{
			// An explicit --sub is the user naming the pin, so nothing was
			// dropped behind their back and there is nothing to explain.
			name:     "a positional with --sub is not a cleared pin",
			args:     claudeRouteArgs{positional: []string{"anthropic"}, sub: "waired", subSet: true},
			wantMain: "anthropic", wantSub: "waired", wantClear: false,
		},
		{
			name:     "--sub same is asked for, not inferred",
			args:     claudeRouteArgs{sub: "same", subSet: true},
			wantMain: "<unchanged>", wantSub: "same", wantClear: false,
		},
		{
			name:     "--main and --sub together",
			args:     claudeRouteArgs{main: "auto", mainSet: true, sub: "anthropic", subSet: true},
			wantMain: "auto", wantSub: "anthropic",
		},
		{
			name:     "the old flag name still works",
			args:     claudeRouteArgs{subagents: "waired", subagentsSet: true},
			wantMain: "<unchanged>", wantSub: "waired",
		},
		{
			name:     "the old flag name with a positional",
			args:     claudeRouteArgs{positional: []string{"anthropic"}, subagents: "waired", subagentsSet: true},
			wantMain: "anthropic", wantSub: "waired", wantClear: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := planClaudeRouteChange(tc.args)
			if err != nil {
				t.Fatalf("planClaudeRouteChange: %v", err)
			}
			if plan.show != tc.wantShow {
				t.Fatalf("show = %v, want %v", plan.show, tc.wantShow)
			}
			if tc.wantShow {
				return
			}
			if got := routeOf(t, plan.req.Main); got != tc.wantMain {
				t.Errorf("main = %s, want %s", got, tc.wantMain)
			}
			if got := routeOf(t, plan.req.Sub); got != tc.wantSub {
				t.Errorf("sub = %s, want %s", got, tc.wantSub)
			}
			if plan.clearsPin != tc.wantClear {
				t.Errorf("clearsPin = %v, want %v", plan.clearsPin, tc.wantClear)
			}
		})
	}
}

func TestPlanClaudeRouteChange_Rejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		args claudeRouteArgs
		want string
	}{
		{
			name: "the positional and --main say the same thing twice",
			args: claudeRouteArgs{positional: []string{"auto"}, main: "waired", mainSet: true},
			want: "use one",
		},
		{
			name: "both spellings of the subagent flag",
			args: claudeRouteArgs{sub: "waired", subSet: true, subagents: "auto", subagentsSet: true},
			want: "use --sub",
		},
		{
			name: "an unknown positional route",
			args: claudeRouteArgs{positional: []string{"openai"}},
			want: "unknown route",
		},
		{
			name: "an unknown --main route",
			args: claudeRouteArgs{main: "openai", mainSet: true},
			want: "unknown route",
		},
		{
			name: "an unknown --sub route",
			args: claudeRouteArgs{sub: "openai", subSet: true},
			want: "unknown route",
		},
		{
			// "same" means "follow the main conversation"; there is no
			// main conversation to follow for the main conversation.
			name: "same is not a main-conversation route",
			args: claudeRouteArgs{positional: []string{"same"}},
			want: "unknown route",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := planClaudeRouteChange(tc.args)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// The note has to name the pin that went away and how to get it back;
// without both it is a message the reader can do nothing with.
func TestClaudeSubPinClearedNote(t *testing.T) {
	got := claudeSubPinClearedNote(state.ClaudeRouteWaired)
	for _, want := range []string{"waired", "--sub waired", "cleared"} {
		if !strings.Contains(got, want) {
			t.Errorf("note %q does not mention %q", got, want)
		}
	}
}

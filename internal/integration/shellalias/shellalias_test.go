package shellalias

import (
	"path/filepath"
	"testing"
)

func TestRCCandidates_DeterministicOrder(t *testing.T) {
	got := RCCandidates("/h")
	want := []RC{
		{Path: filepath.Join("/h", ".bashrc")},
		{Path: filepath.Join("/h", ".zshrc")},
		{Path: filepath.Join("/h", ".config", "fish", "config.fish"), Fish: true},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestFindBlock(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"missing", "# user\nalias x='y'\n", false},
		{"open-only", SentinelOpen + "\nalias claude='waired claude'\n", false},
		{"both", "pre\n" + SentinelOpen + "\nalias claude='/p/waired claude'\n" + SentinelClose + "\npost\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, e, ok := FindBlock([]byte(tc.in))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			snippet := tc.in[s:e]
			if snippet[:len(SentinelOpen)] != SentinelOpen {
				t.Errorf("span does not start at open sentinel; got %q", snippet[:len(SentinelOpen)])
			}
		})
	}
}

func TestExtractCommand_Posix(t *testing.T) {
	block := SentinelOpen + "\n" + `alias claude='/usr/local/bin/waired claude'` + "\n" + SentinelClose + "\n"
	got := ExtractCommand([]byte(block), false)
	if got != "/usr/local/bin/waired claude" {
		t.Errorf("got %q", got)
	}
}

func TestExtractCommand_PosixWithApostrophe(t *testing.T) {
	// Path containing an apostrophe — escaped via '\''.
	block := SentinelOpen + "\n" + `alias claude='/o'\''dd/waired claude'` + "\n" + SentinelClose + "\n"
	got := ExtractCommand([]byte(block), false)
	if got != `/o'dd/waired claude` {
		t.Errorf("got %q", got)
	}
}

func TestExtractCommand_Fish(t *testing.T) {
	block := SentinelOpen + "\n" + `function claude --description 'route claude through waired'; '/p/waired' claude $argv; end` + "\n" + SentinelClose + "\n"
	got := ExtractCommand([]byte(block), true)
	if got != "/p/waired claude" {
		t.Errorf("got %q", got)
	}
}

func TestExtractCommand_FishUnquoted(t *testing.T) {
	block := SentinelOpen + "\n" + `function claude --description 'route claude through waired'; /usr/local/bin/waired claude $argv; end` + "\n" + SentinelClose + "\n"
	got := ExtractCommand([]byte(block), true)
	if got != "/usr/local/bin/waired claude" {
		t.Errorf("got %q", got)
	}
}

func TestExtractCommand_Empty(t *testing.T) {
	got := ExtractCommand([]byte(SentinelOpen+"\n"+SentinelClose+"\n"), false)
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

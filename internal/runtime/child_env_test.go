package runtime

import (
	"reflect"
	"testing"
)

func TestChildBaseEnv(t *testing.T) {
	const sep = ":" // unix; the Windows case sets its own below

	cases := []struct {
		name         string
		goos         string
		parent       []string
		homeFallback string
		extraPathDir string
		pathSep      string
		want         []string
	}{
		{
			name:         "injects HOME when the launcher has none",
			goos:         "darwin",
			parent:       []string{"FOO=1"},
			homeFallback: "/state",
			pathSep:      sep,
			want:         []string{"FOO=1", "HOME=/state"},
		},
		{
			name:         "drops an empty inherited HOME so ours wins",
			goos:         "darwin",
			parent:       []string{"HOME=", "FOO=1"},
			homeFallback: "/state",
			pathSep:      sep,
			want:         []string{"FOO=1", "HOME=/state"},
		},
		{
			name:         "preserves a non-empty inherited HOME (guard, not override)",
			goos:         "linux",
			parent:       []string{"HOME=/home/waired", "FOO=1"},
			homeFallback: "/state",
			pathSep:      sep,
			want:         []string{"HOME=/home/waired", "FOO=1"},
		},
		{
			name:         "never fabricates HOME when homeFallback is empty",
			goos:         "linux",
			parent:       []string{"FOO=1"},
			homeFallback: "",
			pathSep:      sep,
			want:         []string{"FOO=1"},
		},
		{
			name:         "prepends extraPathDir to an existing PATH",
			goos:         "linux",
			parent:       []string{"PATH=/usr/bin:/bin", "FOO=1"},
			extraPathDir: "/opt/engine/bin",
			pathSep:      sep,
			want:         []string{"FOO=1", "PATH=/opt/engine/bin:/usr/bin:/bin"},
		},
		{
			name:         "sets PATH when the parent has none",
			goos:         "linux",
			parent:       []string{"FOO=1"},
			extraPathDir: "/opt/engine/bin",
			pathSep:      sep,
			want:         []string{"FOO=1", "PATH=/opt/engine/bin"},
		},
		{
			name:         "no PATH change when extraPathDir is empty",
			goos:         "linux",
			parent:       []string{"PATH=/usr/bin", "FOO=1"},
			extraPathDir: "",
			pathSep:      sep,
			want:         []string{"PATH=/usr/bin", "FOO=1"},
		},
		{
			name:         "windows matches Path case-insensitively",
			goos:         "windows",
			parent:       []string{`Path=C:\Windows`, "FOO=1"},
			extraPathDir: `C:\engine`,
			pathSep:      ";",
			want:         []string{"FOO=1", `PATH=C:\engine;C:\Windows`},
		},
		{
			name:         "unix treats Path (wrong case) as an ordinary var",
			goos:         "linux",
			parent:       []string{"Path=/weird", "FOO=1"},
			extraPathDir: "/opt/engine/bin",
			pathSep:      sep,
			want:         []string{"Path=/weird", "FOO=1", "PATH=/opt/engine/bin"},
		},
		{
			name:         "both HOME and PATH at once",
			goos:         "darwin",
			parent:       []string{"PATH=/usr/bin:/bin"},
			homeFallback: "/state",
			extraPathDir: "/Applications/Ollama.app/Contents/Resources",
			pathSep:      sep,
			want: []string{
				"HOME=/state",
				"PATH=/Applications/Ollama.app/Contents/Resources:/usr/bin:/bin",
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ChildBaseEnv(c.goos, c.parent, c.homeFallback, c.extraPathDir, c.pathSep)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("ChildBaseEnv(%s) =\n  %v\nwant\n  %v", c.goos, got, c.want)
			}
		})
	}
}

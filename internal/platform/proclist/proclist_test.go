package proclist

import "testing"

func TestIsRunnerProc(t *testing.T) {
	cases := []struct {
		name string
		argv []string
		want bool
	}{
		{"llama-server-unix", []string{"/usr/lib/ollama/runners/llama-server", "-c", "262144"}, true},
		{"llama-server-windows", []string{`C:\Users\x\AppData\Local\Programs\Ollama\llama-server.exe`, "-c", "262144"}, true},
		{"ollama-runner", []string{"/usr/local/bin/ollama", "runner", "--model", "/blobs/sha256-abc"}, true},
		{"ollama-serve-not-runner", []string{"/usr/local/bin/ollama", "serve"}, false},
		{"unrelated", []string{"/usr/bin/python3", "-m", "vllm"}, false},
		{"empty", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsRunnerProc(c.argv); got != c.want {
				t.Errorf("IsRunnerProc(%v) = %v, want %v", c.argv, got, c.want)
			}
		})
	}
}

func TestParseRunnerFlags(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		np, ctx int
		model   string
	}{
		{
			// The exact runner from waired#763 (Windows host).
			name: "issue-763-llama-server-exe",
			argv: []string{
				"llama-server.exe", "--model", `C:\Users\x\.ollama\blobs\sha256-deadbeef`,
				"-c", "262144", "-np", "1", "--spec-type", "draft-mtp",
			},
			np: 1, ctx: 262144, model: `C:\Users\x\.ollama\blobs\sha256-deadbeef`,
		},
		{
			name: "long-flags-ollama-runner",
			argv: []string{"ollama", "runner", "--ctx-size", "524288", "--parallel", "2", "--model", "/blobs/sha256-x"},
			np:   2, ctx: 524288, model: "/blobs/sha256-x",
		},
		{
			name: "inline-equals",
			argv: []string{"llama-server", "-c=131072", "-np=4"},
			np:   4, ctx: 131072,
		},
		{
			name: "flag-without-value-at-end",
			argv: []string{"llama-server", "-c"},
			np:   0, ctx: 0,
		},
		{
			name: "non-numeric-ignored",
			argv: []string{"llama-server", "-c", "big", "-np", "1"},
			np:   1, ctx: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := ParseRunnerFlags(c.argv)
			if f.NumParallel != c.np || f.ContextLen != c.ctx || f.ModelPath != c.model {
				t.Errorf("ParseRunnerFlags(%v) = {np:%d ctx:%d model:%q}, want {np:%d ctx:%d model:%q}",
					c.argv, f.NumParallel, f.ContextLen, f.ModelPath, c.np, c.ctx, c.model)
			}
		})
	}
}

func TestParseProcCmdline(t *testing.T) {
	raw := []byte("llama-server\x00-c\x00262144\x00-np\x001\x00")
	got := parseProcCmdline(raw)
	want := []string{"llama-server", "-c", "262144", "-np", "1"}
	if len(got) != len(want) {
		t.Fatalf("parseProcCmdline len = %d (%v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if parseProcCmdline(nil) != nil {
		t.Error("empty cmdline should yield nil argv")
	}
}

func TestParseCimJSON(t *testing.T) {
	t.Run("array", func(t *testing.T) {
		raw := []byte(`[{"ProcessId":10,"CommandLine":"llama-server.exe -c 262144 -np 1"},` +
			`{"ProcessId":20,"CommandLine":null},` +
			`{"ProcessId":30,"CommandLine":"C:\\ollama.exe serve"}]`)
		got, err := parseCimJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		// The null-CommandLine row is dropped.
		if len(got) != 2 {
			t.Fatalf("got %d procs, want 2: %+v", len(got), got)
		}
		if got[0].PID != 10 || got[0].Argv[0] != "llama-server.exe" || got[0].Argv[2] != "262144" {
			t.Errorf("row0 = %+v", got[0])
		}
	})
	t.Run("single-object", func(t *testing.T) {
		raw := []byte(`{"ProcessId":42,"CommandLine":"llama-server -c 131072 -np 2"}`)
		got, err := parseCimJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].PID != 42 {
			t.Fatalf("got %+v, want single pid 42", got)
		}
		f := ParseRunnerFlags(got[0].Argv)
		if f.ContextLen != 131072 || f.NumParallel != 2 {
			t.Errorf("flags = %+v", f)
		}
	})
	t.Run("empty", func(t *testing.T) {
		got, err := parseCimJSON([]byte("  "))
		if err != nil || got != nil {
			t.Errorf("empty = (%v, %v), want (nil, nil)", got, err)
		}
	})
}

func TestSplitWindowsCmdline(t *testing.T) {
	got := splitWindowsCmdline(`"C:\Program Files\Ollama\llama-server.exe" -c 262144 -np 1`)
	want := []string{`C:\Program Files\Ollama\llama-server.exe`, "-c", "262144", "-np", "1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("token[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParsePsOutput(t *testing.T) {
	raw := []byte("  501 /usr/local/bin/ollama runner --ctx-size 262144 --parallel 1\n" +
		"  777 /usr/bin/vim\n")
	got := parsePsOutput(raw)
	if len(got) != 2 {
		t.Fatalf("got %d procs, want 2: %+v", len(got), got)
	}
	if got[0].PID != 501 || !IsRunnerProc(got[0].Argv) {
		t.Errorf("row0 = %+v, want runner pid 501", got[0])
	}
	if f := ParseRunnerFlags(got[0].Argv); f.ContextLen != 262144 || f.NumParallel != 1 {
		t.Errorf("flags = %+v", f)
	}
}

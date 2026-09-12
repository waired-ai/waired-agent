package proclist

import (
	"reflect"
	"strings"
	"testing"
)

// macRunnerCommand is the command line `ps -axww -o command=` printed for
// the live runner on pc-mbp14-m5 and sv-macmini on 2026-09-12, truncated
// after the flags this package reads. Every macOS install produces this
// shape, because the bundled engine lives under
// /Library/Application Support/waired (internal/platform/paths).
const macRunnerCommand = `/Library/Application Support/waired/runtimes/ollama/bin/llama-server ` +
	`--model /Library/Application Support/waired/runtimes/ollama/models/blobs/sha256-d372de8e ` +
	`--port 53960 --host 127.0.0.1 --no-webui --offline -c 200704 -np 1 --log-verbosity 4`

const macRunnerProgram = `/Library/Application Support/waired/runtimes/ollama/bin/llama-server`

// TestArgvWithProgram pins how a command-line string and a separately
// reported program path are joined into argv.
//
// Product contract, ratifying source waired-agent#1303: the program path is
// argv[0] only when the command line actually starts with it. It takes no
// GOOS parameter on purpose — once each list() supplies Program there is no
// OS-varying DECISION left, only OS-varying INPUTS, which the rows below
// carry (CLAUDE.md's initStateDirMode model is about the former).
func TestArgvWithProgram(t *testing.T) {
	winProgram := `C:\Users\a b\AppData\Roaming\waired\runtimes\ollama\bin\llama-server.exe`
	cases := []struct {
		name     string
		program  string
		cmdline  string
		split    func(string) []string
		want     []string
		isRunner bool
	}{
		{
			name:     "darwin-space-bearing-prefix",
			program:  macRunnerProgram,
			cmdline:  macRunnerCommand,
			split:    strings.Fields,
			want:     append([]string{macRunnerProgram}, strings.Fields(macRunnerCommand[len(macRunnerProgram)+1:])...),
			isRunner: true,
		},
		{
			name:     "darwin-space-free",
			program:  "/usr/local/bin/ollama",
			cmdline:  "/usr/local/bin/ollama runner --ctx-size 262144",
			split:    strings.Fields,
			want:     []string{"/usr/local/bin/ollama", "runner", "--ctx-size", "262144"},
			isRunner: true,
		},
		{
			name:     "program-only-no-arguments",
			program:  "/usr/local/bin/ollama",
			cmdline:  "/usr/local/bin/ollama",
			split:    strings.Fields,
			want:     []string{"/usr/local/bin/ollama"},
			isRunner: false, // `ollama` with no `runner` argument
		},
		{
			name:     "windows-unquoted-space-bearing",
			program:  winProgram,
			cmdline:  winProgram + " --model C:\\blobs\\sha256-abc -c 401408 -np 2",
			split:    splitWindowsCmdline,
			want:     []string{winProgram, "--model", `C:\blobs\sha256-abc`, "-c", "401408", "-np", "2"},
			isRunner: true,
		},
		{
			name:     "windows-quoted-space-bearing",
			program:  winProgram,
			cmdline:  `"` + winProgram + `" --model C:\blobs\sha256-abc -np 2`,
			split:    splitWindowsCmdline,
			want:     []string{winProgram, "--model", `C:\blobs\sha256-abc`, "-np", "2"},
			isRunner: true,
		},
		{
			name:     "no-program-falls-back-to-the-split",
			program:  "",
			cmdline:  "/usr/local/bin/ollama runner --parallel 1",
			split:    strings.Fields,
			want:     []string{"/usr/local/bin/ollama", "runner", "--parallel", "1"},
			isRunner: true, // argv[0] is space-free, so the old path still works
		},
		{
			// A process that rewrote its own argv[0], or a PID reused
			// between the two ps reads. Never staple the program path on.
			name:    "argv0-rewritten-does-not-match",
			program: "/usr/local/bin/ollama",
			cmdline: "postgres: writer process",
			split:   strings.Fields,
			want:    []string{"postgres:", "writer", "process"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := argvWithProgram(c.program, c.cmdline, c.split)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("argvWithProgram()\n got %q\nwant %q", got, c.want)
			}
			// The point of rebuilding argv[0] is that the ordinary
			// predicate works again on it.
			if IsRunnerProc(got) != c.isRunner {
				t.Errorf("IsRunnerProc(%q) = %v, want %v", got, IsRunnerProc(got), c.isRunner)
			}
		})
	}
}

// TestMacRunnerIdentifiesOnceArgv0IsRebuilt is the defect itself, in one
// test: whitespace-splitting the macOS command line loses the runner, and
// joining the OS-reported program path back on recovers it — along with
// the -np the whole issue turns on.
func TestMacRunnerIdentifiesOnceArgv0IsRebuilt(t *testing.T) {
	shattered := strings.Fields(macRunnerCommand)
	if IsRunnerProc(shattered) {
		t.Fatalf("argv[0] %q identified a runner; the fixture no longer reproduces waired-agent#1303", shattered[0])
	}
	argv := argvWithProgram(macRunnerProgram, macRunnerCommand, strings.Fields)
	if !IsRunnerProc(argv) {
		t.Error("IsRunnerProc = false for the live macOS runner command line")
	}
	if f := ParseRunnerFlags(argv); f.NumParallel != 1 || f.ContextLen != 200704 {
		t.Errorf("ParseRunnerFlags = %+v, want -np 1 -c 200704", f)
	}
}

// TestMergePsReads pins the join between the two `ps` reads.
//
// Product contract, ratifying source waired-agent#1303: the join is by PID.
// The two reads are two process-table snapshots, so they neither line up by
// position nor contain the same set of PIDs.
func TestMergePsReads(t *testing.T) {
	command := []byte("" +
		"  86961 " + macRunnerCommand + "\n" +
		"    777 /usr/bin/vim /tmp/a.txt\n" +
		"   4242 /usr/local/bin/ollama serve\n")
	// `pid ppid comm`, deliberately in a different order from the first
	// read, and missing 777 — a process that exited between the two.
	comm := []byte("" +
		"   4242     1 /usr/local/bin/ollama\n" +
		"  86961  4242 " + macRunnerProgram + "\n" +
		"   9999     1 /usr/sbin/cupsd\n")

	got := mergePsReads(command, comm)
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(got), got)
	}
	byPID := map[int]ProcInfo{}
	for _, p := range got {
		byPID[p.PID] = p
	}
	runner, ok := byPID[86961]
	if !ok {
		t.Fatal("runner pid 86961 missing from the merge")
	}
	if runner.Program != macRunnerProgram {
		t.Errorf("Program = %q, want the comm= value", runner.Program)
	}
	if !IsRunnerProc(runner.Argv) {
		t.Error("merged runner row did not identify as a runner")
	}
	if runner.PPID != 4242 {
		t.Errorf("PPID = %d, want 4242 — the parent rides the same read", runner.PPID)
	}
	// 777 was only in the first read: no program path, and the pre-#1303
	// split, rather than a row stapled to another process's path.
	vim := byPID[777]
	if vim.Program != "" {
		t.Errorf("pid 777 Program = %q, want empty (absent from the comm read)", vim.Program)
	}
	if len(vim.Argv) == 0 || vim.Argv[0] != "/usr/bin/vim" {
		t.Errorf("pid 777 argv = %q", vim.Argv)
	}
	// 9999 was only in the second read: there is no argv for it, so it is
	// not a process this package can report.
	if _, present := byPID[9999]; present {
		t.Error("pid 9999 appeared with no command line")
	}
}

// TestMergePsReads_NoSecondRead is the degradation path: a failed or
// unsupported `ps -o comm=` must leave the single-read behaviour intact.
func TestMergePsReads_NoSecondRead(t *testing.T) {
	command := []byte("  501 /usr/local/bin/ollama runner --ctx-size 262144 --parallel 1\n")
	got := mergePsReads(command, nil)
	if len(got) != 1 || got[0].PID != 501 || got[0].Program != "" {
		t.Fatalf("got %+v", got)
	}
	if !IsRunnerProc(got[0].Argv) {
		t.Error("space-free runner must still identify with no program path")
	}
}

// TestParseCimJSON_ExecutablePath pins the Windows half of the same fix.
// Ollama spawns the runner UNQUOTED, so an install under a path with a
// space — %AppData%\waired when the profile name has one — shatters argv[0]
// exactly as it did on macOS unless ExecutablePath is read
// (waired-agent#1303).
func TestParseCimJSON_ExecutablePath(t *testing.T) {
	const prog = `C:\Users\a b\AppData\Roaming\waired\runtimes\ollama\bin\llama-server.exe`
	t.Run("space-bearing-unquoted", func(t *testing.T) {
		raw := []byte(`[{"ProcessId":1536,` +
			`"CommandLine":"` + jsonEscape(prog+` --model C:\blobs\sha256-abc -c 401408 -np 2`) + `",` +
			`"ExecutablePath":"` + jsonEscape(prog) + `"}]`)
		got, err := parseCimJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d rows: %+v", len(got), got)
		}
		if got[0].Program != prog {
			t.Errorf("Program = %q, want %q", got[0].Program, prog)
		}
		if !IsRunnerProc(got[0].Argv) {
			t.Error("IsRunnerProc = false for a space-bearing unquoted Windows runner")
		}
		if f := ParseRunnerFlags(got[0].Argv); f.NumParallel != 2 || f.ContextLen != 401408 {
			t.Errorf("flags = %+v, want -np 2 -c 401408", f)
		}
	})
	t.Run("null-executable-path-falls-back", func(t *testing.T) {
		raw := []byte(`{"ProcessId":9,"CommandLine":"llama-server.exe -c 1024 -np 1","ExecutablePath":null}`)
		got, err := parseCimJSON(raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Program != "" {
			t.Fatalf("got %+v, want one row with no program path", got)
		}
		if !IsRunnerProc(got[0].Argv) {
			t.Error("a space-free Windows runner must still identify with no ExecutablePath")
		}
	})
}

// jsonEscape renders a Windows path for embedding in a JSON string literal.
func jsonEscape(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }

// TestLiveRunners is the second half of what went wrong on sv-macmini
// (waired-agent#1303): a runner orphaned fourteen hours earlier carried the
// same window and the same -np as the live one, so a caller looking for a
// UNIQUE match found two and abstained — and the host then advertised a
// figure nothing had measured.
//
// Product contract, ratifying source waired-agent#1303: a runner belongs to
// the engine that is serving only while the ollama that spawned it is still
// there.
func TestLiveRunners(t *testing.T) {
	const runner = "/Library/Application Support/waired/runtimes/ollama/bin/llama-server"
	const engine = "/Library/Application Support/waired/runtimes/ollama/bin/ollama"
	procs := []ProcInfo{
		// the orphan: its ollama died, launchd adopted it
		{PID: 6000, PPID: 1, Program: runner, Argv: []string{runner, "-c", "200704", "-np", "1"}},
		// the live engine and its runner
		{PID: 35576, PPID: 35557, Program: engine, Argv: []string{engine, "serve"}},
		{PID: 35709, PPID: 35576, Program: runner, Argv: []string{runner, "-c", "200704", "-np", "2"}},
		// not a runner at all
		{PID: 777, PPID: 1, Argv: []string{"/usr/bin/vim"}},
	}
	got := LiveRunners(procs)
	if len(got) != 1 {
		t.Fatalf("got %d live runners, want 1: %+v", len(got), got)
	}
	if got[0].PID != 35709 {
		t.Errorf("live runner = pid %d, want 35709 (the orphan 6000 was kept)", got[0].PID)
	}

	t.Run("a platform that reports no parent keeps every runner", func(t *testing.T) {
		// Linux before this read existed, and any OS whose ps declined the
		// column: the distinction cannot be made, so nothing is dropped
		// and the behaviour is exactly what it was.
		none := []ProcInfo{
			{PID: 1, Argv: []string{runner, "-c", "200704", "-np", "1"}},
			{PID: 2, Argv: []string{runner, "-c", "4096", "-np", "1"}},
		}
		if got := LiveRunners(none); len(got) != 2 {
			t.Errorf("got %d, want 2 — an unknown parent must not exclude a runner", len(got))
		}
	})

	t.Run("the in-process runner is its own engine", func(t *testing.T) {
		// `ollama runner` has no separate `ollama serve` parent to find.
		inproc := []ProcInfo{
			{PID: 9, PPID: 1, Argv: []string{engine, "runner", "--ctx-size", "4096"}},
		}
		if got := LiveRunners(inproc); len(got) != 1 {
			t.Errorf("got %d, want 1 — the in-process runner was dropped for having no engine parent", len(got))
		}
	})
}

// TestIsOllamaServe keeps the parent test honest about what it matches.
func TestIsOllamaServe(t *testing.T) {
	for _, c := range []struct {
		argv []string
		want bool
	}{
		{[]string{"/usr/local/bin/ollama", "serve"}, true},
		{[]string{`C:\ProgramData\waired\runtimes\ollama\bin\ollama.exe`, "serve"}, true},
		{[]string{"/usr/local/bin/ollama", "runner"}, false},
		{[]string{"/usr/local/bin/ollama"}, false},
		{[]string{"/usr/lib/ollama/llama-server", "-np", "1"}, false},
		{nil, false},
	} {
		if got := IsOllamaServe(c.argv); got != c.want {
			t.Errorf("IsOllamaServe(%q) = %v, want %v", c.argv, got, c.want)
		}
	}
}

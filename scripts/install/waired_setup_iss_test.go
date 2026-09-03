package installscripts

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The Windows GUI installer is an Inno Setup script, and until waired-agent#1181
// nothing in CI read it: install-script-lint is shellcheck over the .sh files,
// ps-script-lint covers the .ps1 files, and the install test's ExeVariant leg
// only observes the effects of a run on a Windows runner. So a change to
// waired-setup.iss could move where a program is executed from -- which decides
// whether a failure can still fail the installation -- and no check would
// notice until someone read the file.
//
// These tests are a record of the shape #1181 landed on, not a style rule. What
// they pin, and why each one matters, is on each test.
const issRel = "packaging/windows/waired-setup.iss"

func readISS(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(issRel)))
	if err != nil {
		t.Fatalf("read %s: %v", issRel, err)
	}
	return string(b)
}

// section returns the body of one [Section] of an Inno Setup script: every line
// after the header up to the next one. Continuation lines (a trailing "\") are
// joined, because Inno treats them as one entry and so must anything reading
// them.
func section(t *testing.T, iss, name string) []string {
	t.Helper()
	var (
		out   []string
		in    bool
		joint string
	)
	header := regexp.MustCompile(`^\[[A-Za-z]+\]\s*$`)
	for _, raw := range strings.Split(iss, "\n") {
		line := strings.TrimRight(raw, "\r")
		if header.MatchString(strings.TrimSpace(line)) {
			in = strings.EqualFold(strings.TrimSpace(line), "["+name+"]")
			continue
		}
		if !in {
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasSuffix(trimmed, `\`) {
			joint += strings.TrimSpace(strings.TrimSuffix(trimmed, `\`)) + " "
			continue
		}
		out = append(out, strings.TrimSpace(joint+trimmed))
		joint = ""
	}
	if len(out) == 0 {
		t.Fatalf("[%s] section of %s is empty -- the parser or the file changed shape", name, issRel)
	}
	return out
}

// entryField pulls one "Name: value" field out of an Inno section entry.
// Values are quoted in this script; both forms are accepted so a future
// unquoted value is read rather than silently dropped.
func entryField(entry, field string) string {
	re := regexp.MustCompile(`(?i)\b` + field + `:\s*("([^"]*)"|[^;]*)`)
	m := re.FindStringSubmatch(entry)
	if m == nil {
		return ""
	}
	if m[2] != "" || strings.HasPrefix(strings.TrimSpace(m[1]), `"`) {
		return m[2]
	}
	return strings.TrimSpace(m[1])
}

// TestSetupRunsOnlyTheseProgramsFromRunSections pins what the installer
// executes from [Run] and [UninstallRun].
//
// The point is the emptiness of [Run], not its contents. Inno processes [Run]
// after the install stage has been committed and discards the result
// (Setup.MainForm.pas ProcessRunEntries), so a program that fails there leaves
// Setup reporting success -- which is exactly what #1181 was: a blocked
// `waired-agent.exe install` was logged and stepped over, and the Claude Code
// integration ran anyway. Anything that must be able to fail the install
// belongs in [Code], reached from a [Files] AfterInstall.
func TestSetupRunsOnlyTheseProgramsFromRunSections(t *testing.T) {
	iss := readISS(t)

	want := map[string][]string{
		"Run": {
			// The tray, launched from the finish page. Nothing else: a [Run]
			// entry cannot fail the installation.
			`{app}\waired-tray.exe `,
		},
		"UninstallRun": {
			`{app}\waired.exe claude disable`,
			`{app}\waired-agent.exe uninstall`,
		},
	}
	for _, name := range []string{"Run", "UninstallRun"} {
		var got []string
		for _, entry := range section(t, iss, name) {
			got = append(got, entryField(entry, "Filename")+" "+entryField(entry, "Parameters"))
		}
		sort.Strings(got)
		expect := append([]string(nil), want[name]...)
		sort.Strings(expect)
		if strings.Join(got, "\n") != strings.Join(expect, "\n") {
			t.Errorf("[%s] runs a different set of programs than expected\n got: %q\nwant: %q", name, got, expect)
		}
	}
}

// TestCodeRunsOnlyTheseProgramsDuringSetup pins the programs [Code] executes.
// Every one of them goes through RunInstalledProgram, whose whole job is to
// report a program Windows would not start (Exec returning False) as loudly as
// one that ran and failed -- the two were indistinguishable before #1181
// because neither was looked at.
func TestCodeRunsOnlyTheseProgramsDuringSetup(t *testing.T) {
	iss := readISS(t)

	// The program name is sometimes a string constant, so resolve the script's
	// own `const Name = 'value';` lines before matching the call sites.
	consts := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*([A-Za-z]\w*)\s*=\s*'([^']*)';`).FindAllStringSubmatch(iss, -1) {
		consts[m[1]] = m[2]
	}
	re := regexp.MustCompile(`RunInstalledProgram\((?:'([^']+)'|([A-Za-z]\w*)),\s*'([^']*)'\)`)
	seen := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(iss, -1) {
		name := m[1]
		if name == "" {
			var ok bool
			if name, ok = consts[m[2]]; !ok {
				t.Fatalf("RunInstalledProgram is called with %s, which this guard cannot resolve to a program name", m[2])
			}
		}
		seen[name+" "+m[3]] = true
	}
	want := []string{
		"waired-agent.exe install",   // fresh install: register the service
		"waired-agent.exe start",     // and bring it up; its exit code is the answer
		"waired-agent.exe stop",      // upgrade: stop the old one before replacing it
		"waired-agent.exe uninstall", // a fresh install that failed leaves no half-registered service
		"waired.exe claude enable",   // last, and only with a running daemon
	}
	var got []string
	for k := range seen {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("[Code] runs a different set of programs than expected\n got: %q\nwant: %q", got, want)
	}
}

// TestClaudeCodeIsTouchedOnlyAfterTheServiceIsRunning is the #1181 defect
// itself: Setup pointed Claude Code at a gateway that would never listen,
// because the integration step did not depend on the service existing.
func TestClaudeCodeIsTouchedOnlyAfterTheServiceIsRunning(t *testing.T) {
	iss := readISS(t)

	enable := strings.Index(iss, `RunInstalledProgram('waired.exe', 'claude enable')`)
	if enable < 0 {
		t.Fatal("the Claude Code integration step is gone: GUI installs would be left unrouted")
	}
	guard := strings.Index(iss, "if not gAgentRunning then")
	if guard < 0 {
		t.Fatal("nothing checks gAgentRunning before the Claude Code integration (#1181)")
	}
	if guard > enable {
		t.Error("the Claude Code integration runs before the service is checked (#1181)")
	}
	if !strings.Contains(iss, "gAgentRunning := True") {
		t.Error("gAgentRunning is never set: the integration would never run")
	}
}

// TestProgramsAreEmbeddedOnceAndTriedBeforeTheyAreInstalled pins the
// arrangement that lets Setup try the programs before it installs them: all
// three embedded once with dontcopy, so PrepareToInstall can extract and run
// them; the two Inno installs taken from those same extracted copies with
// external, so there is no second ~50 MB copy in the setup executable and no
// chance of installing bytes nobody tried.
//
// waired-agent.exe is the exception, and it has to be: PrepareToInstall puts it
// in place and starts its service, because nothing after PrepareToInstall can
// still decline (see the .iss header). A [Files] entry for it would overwrite
// the running service's own image straight afterwards.
func TestProgramsAreEmbeddedOnceAndTriedBeforeTheyAreInstalled(t *testing.T) {
	iss := readISS(t)
	entries := section(t, iss, "Files")

	for _, prog := range []string{"waired.exe", "waired-agent.exe", "waired-tray.exe"} {
		var embedded, byInno bool
		for _, e := range entries {
			switch entryField(e, "Source") {
			case `dist\windows-amd64\` + prog:
				embedded = strings.Contains(entryField(e, "Flags"), "dontcopy")
			case `{tmp}\` + prog:
				byInno = strings.Contains(entryField(e, "Flags"), "external") && entryField(e, "DestDir") == "{app}"
			}
		}
		if !embedded {
			t.Errorf("%s is not embedded with dontcopy: PrepareToInstall cannot try it before installing it", prog)
		}
		wantByInno := prog != "waired-agent.exe"
		if byInno == wantByInno {
			continue
		}
		if wantByInno {
			t.Errorf("%s is not installed from the checked copy in {tmp} with external", prog)
		} else {
			t.Errorf("%s is installed by [Files]; PrepareToInstall places it, and [Files] would overwrite the running service's image", prog)
		}
	}

	// Whatever Inno does not install, the uninstaller has to be told about.
	del := strings.Join(section(t, iss, "UninstallDelete"), "\n")
	for _, name := range []string{`{app}\waired-agent.exe`, `{app}\waired-agent.exe.displaced-*`} {
		if !strings.Contains(del, name) {
			t.Errorf("[UninstallDelete] does not remove %s, and Inno's uninstall log does not know it either", name)
		}
	}
}

// TestNothingHangsOffAnAfterInstall keeps work out of the hooks that cannot fail
// an installation. Inno catches exceptions from Before/AfterInstall on purpose
// -- "Don't allow exceptions raised by Before/AfterInstall functions to be
// propagated out", Setup.MainFunc.pas NotifyInstallEntry -- so a step placed
// there reports its failure to nobody, which is the shape of #1181. Measured on
// Windows 11 with Inno Setup 6.7.3: an AfterInstall that raised left Setup
// exiting 0 with everything installed.
func TestNothingHangsOffAnAfterInstall(t *testing.T) {
	iss := readISS(t)
	for _, sec := range []string{"Files", "Run", "UninstallRun"} {
		for i, e := range section(t, iss, sec) {
			for _, field := range []string{"AfterInstall", "BeforeInstall"} {
				if got := entryField(e, field); got != "" {
					t.Errorf("[%s] entry %d has %s: %q -- that hook cannot fail an install", sec, i, field, got)
				}
			}
		}
	}
	if !strings.Contains(iss, "function PrepareToInstall(") {
		t.Error("PrepareToInstall is gone: nothing left in this script can decline an install")
	}
}

// TestStagedChecksMatchInstallPS1 holds the GUI installer's pre-flight table
// against the PowerShell installer's. They are two copies of one decision
// (docs/decisions/20260829/1730-installer-refuses-programs-that-cannot-run.md)
// and would otherwise drift: a computer would be refused by one installer and
// accepted by the other, which is worse than either answer.
func TestStagedChecksMatchInstallPS1(t *testing.T) {
	root := repoRoot(t)
	iss := readISS(t)

	issRe := regexp.MustCompile(
		`if Name = '([^']+)'\s+then begin Params := '([^']*)';\s+RequireZeroExit := (True|False);\s+Fatal := (True|False);\s+end;`)
	fromISS := map[string]string{}
	for _, m := range issRe.FindAllStringSubmatch(iss, -1) {
		fromISS[m[1]] = fmt.Sprintf("args=%q zero=%s fatal=%s",
			m[2], strings.ToLower(m[3]), strings.ToLower(m[4]))
	}
	if len(fromISS) != 3 {
		t.Fatalf("read %d checks out of %s, want 3 -- StagedCheck changed shape and this guard stopped reading it",
			len(fromISS), issRel)
	}

	const ps1Rel = "packaging/install/install.ps1"
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ps1Rel)))
	if err != nil {
		t.Fatalf("read %s: %v", ps1Rel, err)
	}
	ps1Re := regexp.MustCompile(
		`@\{\s*Name\s*=\s*'([^']+)';\s*Arguments\s*=\s*@\(([^)]*)\);\s*RequireZeroExit\s*=\s*\$(true|false);\s*Fatal\s*=\s*\$(true|false)\s*\}`)
	fromPS1 := map[string]string{}
	for _, m := range ps1Re.FindAllStringSubmatch(string(b), -1) {
		var args []string
		for _, a := range strings.Split(m[2], ",") {
			args = append(args, strings.Trim(strings.TrimSpace(a), "'"))
		}
		fromPS1[m[1]] = fmt.Sprintf("args=%q zero=%s fatal=%s",
			strings.Join(args, " "), strings.ToLower(m[3]), strings.ToLower(m[4]))
	}
	if len(fromPS1) != 3 {
		t.Fatalf("read %d checks out of %s, want 3 -- Get-StagedBinaryChecks changed shape and this guard stopped reading it",
			len(fromPS1), ps1Rel)
	}

	for name, want := range fromPS1 {
		got, ok := fromISS[name]
		if !ok {
			t.Errorf("%s checks %s before installing it; %s does not", ps1Rel, name, issRel)
			continue
		}
		if got != want {
			t.Errorf("%s asks %s differently than %s does\n .iss: %s\n .ps1: %s", issRel, name, ps1Rel, got, want)
		}
	}
	for name := range fromISS {
		if _, ok := fromPS1[name]; !ok {
			t.Errorf("%s checks %s before installing it; %s does not", issRel, name, ps1Rel)
		}
	}
}

// TestSetupAlwaysWritesALog pins SetupLogging. #1181 could only be diagnosed --
// "CreateProcess failed; code 4551." twice, then Setup carrying on -- because
// that run happened to have a log. Without this directive only a caller that
// passes /LOG gets one, which is never the person hitting the bug.
func TestSetupAlwaysWritesALog(t *testing.T) {
	if !regexp.MustCompile(`(?mi)^SetupLogging\s*=\s*yes\s*$`).MatchString(readISS(t)) {
		t.Errorf("%s does not set SetupLogging=yes: a failed install leaves nothing to read", issRel)
	}
}

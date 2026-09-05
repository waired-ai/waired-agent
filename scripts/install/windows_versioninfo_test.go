package installscripts

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf16"
)

// Every Windows program Waired ships used to report itself as version 0.0.0.0,
// and two of the three carried no VERSIONINFO resource at all, so Explorer's
// Properties dialog and Task Manager's Details column said nothing about them
// (waired-agent#1209). Nothing noticed, because nothing in CI has ever read
// either the .json that defines a resource or the .syso generated from it:
// there is no `go generate && git diff --exit-code` step in this repository.
//
// So this guard reads both, and cross-checks them against each other and
// against the set of programs that actually ship.
//
// The shipped set is NOT typed here. It is read off the Makefile's Windows
// staging step — the `cp … $(WIN_DIST_DIR)/<name>.exe` lines that decide what
// goes into the release zip and what the .iss installs. #1209 asked for the
// set to be "checked the way sac-signing-inventory.txt checks the signing
// set", i.e. by set comparison rather than a typed list, and the ledger is
// cross-checked against this one below — but the ledger is deliberately NOT
// the source. It shrinks by design as files get signed (its own header says a
// line disappearing means that file is now signed), and a signed program still
// needs a version resource, so deriving from it would quietly narrow this
// guard on the day waired-ai/waired#759 lands.
const (
	sacInventoryRel = "scripts/dev/testdata/sac-signing-inventory.txt"
	makefileRel     = "Makefile"
	sacMatrixRel    = "scripts/dev/sac-control-matrix.ps1"
)

// placeholderVersion is what the COMMITTED resources carry. A release build
// (`make dist-windows-installer`) regenerates them stamped with the real
// version, which leaves the working tree dirty on purpose; this value is how
// that byproduct is kept out of a commit.
const placeholderVersion = "0.0.0-dev"

// versionInfoJSON is the subset of goversioninfo's schema this guard reads.
type versionInfoJSON struct {
	FixedFileInfo struct {
		FileVersion    versionQuad `json:"FileVersion"`
		ProductVersion versionQuad `json:"ProductVersion"`
	} `json:"FixedFileInfo"`
	StringFileInfo map[string]string `json:"StringFileInfo"`
}

type versionQuad struct {
	Major int `json:"Major"`
	Minor int `json:"Minor"`
	Patch int `json:"Patch"`
	Build int `json:"Build"`
}

func (q versionQuad) String() string {
	return fmt.Sprintf("%d.%d.%d.%d", q.Major, q.Minor, q.Patch, q.Build)
}

// shippedWindowsPrograms reads the Makefile's Windows staging step and returns
// the cmd/<name> of every .exe that goes into the release zip, sorted.
func shippedWindowsPrograms(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(t), makefileRel))
	if err != nil {
		t.Fatalf("read %s: %v", makefileRel, err)
	}
	re := regexp.MustCompile(`\$\(WIN_DIST_DIR\)/([A-Za-z0-9_-]+)\.exe`)
	seen := map[string]bool{}
	var names []string
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		names = append(names, m[1])
	}
	if len(names) == 0 {
		t.Fatalf("no `$(WIN_DIST_DIR)/<name>.exe` staging lines found in %s -- the packaging step moved and this guard now checks nothing", makefileRel)
	}
	sort.Strings(names)
	return names
}

// TestTheSigningLedgerNamesOnlyProgramsWeShip keeps the two lists honest in the
// one direction that stays true. The ledger may legitimately be SMALLER than
// the shipped set — its own header says a line disappearing means that file is
// now signed — but a ledger entry naming a program we do not ship is a stale
// claim that nothing else would catch, and it is the ledger that decides what
// the SAC audit expects to see.
func TestTheSigningLedgerNamesOnlyProgramsWeShip(t *testing.T) {
	shipped := map[string]bool{}
	for _, n := range shippedWindowsPrograms(t) {
		shipped[n] = true
	}
	b, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(sacInventoryRel)))
	if err != nil {
		t.Fatalf("read %s: %v", sacInventoryRel, err)
	}
	found := 0
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		bucket, file, ok := strings.Cut(line, "/")
		if !ok || bucket != "ProgramFiles" || !strings.HasSuffix(file, ".exe") {
			continue
		}
		found++
		if name := strings.TrimSuffix(file, ".exe"); !shipped[name] {
			t.Errorf("%s lists %s, which the Makefile does not stage into the Windows release", sacInventoryRel, line)
		}
	}
	if found == 0 {
		t.Fatalf("%s named no ProgramFiles/*.exe entries -- the ledger format changed and this cross-check now checks nothing", sacInventoryRel)
	}
}

// resourceStrings pulls the UTF-16LE strings out of a .syso. A VERSIONINFO
// resource stores every key and value as a NUL-terminated UTF-16LE string, so
// reading them back needs no COFF parser -- which is the point: this guard
// must be able to see what the committed bytes SAY without depending on the
// generator being installed.
func resourceStrings(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var (
		out   []string
		cur   []uint16
		flush = func() {
			if len(cur) > 0 {
				out = append(out, string(utf16.Decode(cur)))
				cur = cur[:0]
			}
		}
	)
	for i := 0; i+1 < len(b); i += 2 {
		u := binary.LittleEndian.Uint16(b[i:])
		// Printable BMP only: the resource is padded with NULs and the COFF
		// wrapper around it is not text.
		if u >= 0x20 && u != 0xFFFD {
			cur = append(cur, u)
			continue
		}
		flush()
	}
	flush()
	if len(out) == 0 {
		t.Fatalf("%s decoded to no UTF-16 strings -- the resource format changed and this guard now checks nothing", path)
	}
	return out
}

func readVersionInfoJSON(t *testing.T, path string) versionInfoJSON {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var vi versionInfoJSON
	if err := json.Unmarshal(b, &vi); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(vi.StringFileInfo) == 0 {
		t.Fatalf("%s has no StringFileInfo -- the schema changed and this guard now checks nothing", path)
	}
	return vi
}

// TestEveryShippedWindowsProgramHasAVersionResource is the whole of #1209 as
// an assertion: what ships and what carries a resource are the same set.
func TestEveryShippedWindowsProgramHasAVersionResource(t *testing.T) {
	root := repoRoot(t)
	for _, name := range shippedWindowsPrograms(t) {
		t.Run(name, func(t *testing.T) {
			for _, rel := range []string{
				filepath.Join("cmd", name, "versioninfo.json"),
				filepath.Join("cmd", name, "resource_windows_amd64.syso"),
			} {
				if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
					t.Errorf("%s ships on Windows but %s is missing: %v", name+".exe", filepath.ToSlash(rel), err)
				}
			}
		})
	}
}

// TestVersionResourceNamesItsOwnProgram pins the fields Explorer and Task
// Manager read. FileDescription is the one Task Manager's Details column
// shows, and it is the reason the tray has a resource at all (waired#810);
// the CLI and the daemon use the names those two surfaces already use
// elsewhere -- the Start Menu entry and the Windows service display name.
func TestVersionResourceNamesItsOwnProgram(t *testing.T) {
	root := repoRoot(t)
	wantDescription := map[string]string{
		"waired":       "Waired (CLI)", // packaging/windows/waired-setup.iss's Start Menu entry
		"waired-agent": "Waired Agent", // internal/platform/service.DisplayName
		"waired-tray":  "Waired",       // waired#810
	}
	for _, name := range shippedWindowsPrograms(t) {
		t.Run(name, func(t *testing.T) {
			vi := readVersionInfoJSON(t, filepath.Join(root, "cmd", name, "versioninfo.json"))
			s := vi.StringFileInfo
			if got, want := s["OriginalFilename"], name+".exe"; got != want {
				t.Errorf("OriginalFilename = %q, want %q", got, want)
			}
			if got := s["InternalName"]; got != name {
				t.Errorf("InternalName = %q, want %q", got, name)
			}
			for _, key := range []string{"CompanyName", "FileDescription", "ProductName"} {
				if strings.TrimSpace(s[key]) == "" {
					t.Errorf("%s is empty -- an empty field shows as blank in Explorer, which is what #1209 is about", key)
				}
			}
			if want, ok := wantDescription[name]; ok && s["FileDescription"] != want {
				t.Errorf("FileDescription = %q, want %q -- this string is what Task Manager's Details column shows, and it is meant to match the name the user already sees elsewhere",
					s["FileDescription"], want)
			}
		})
	}
}

// TestCommittedResourceMatchesItsJSON is the freshness check nothing else
// performs. The .syso is a generated file that is committed, and no CI step
// runs the generator, so an edit to versioninfo.json that nobody regenerated
// would ship the OLD strings for ever, silently.
func TestCommittedResourceMatchesItsJSON(t *testing.T) {
	root := repoRoot(t)
	for _, name := range shippedWindowsPrograms(t) {
		t.Run(name, func(t *testing.T) {
			vi := readVersionInfoJSON(t, filepath.Join(root, "cmd", name, "versioninfo.json"))
			got := resourceStrings(t, filepath.Join(root, "cmd", name, "resource_windows_amd64.syso"))
			// Pair by ADJACENCY, not by membership. A VERSIONINFO String
			// structure stores each key immediately followed by its value, and
			// a membership test passes as soon as the string exists anywhere in
			// the resource -- which it usually does: renaming FileDescription
			// from "Waired (CLI)" to "waired" went unnoticed by exactly that
			// version of this test, because "waired" was already in there as
			// InternalName.
			for key, value := range vi.StringFileInfo {
				if value == "" {
					// goversioninfo omits empty values (LegalCopyright), so
					// there is nothing to find for them.
					continue
				}
				at := -1
				for i, s := range got {
					if s == key {
						at = i
						break
					}
				}
				if at < 0 {
					t.Errorf("versioninfo.json declares %s but the committed .syso has no such key -- run `make versioninfo`", key)
					continue
				}
				if at+1 >= len(got) || got[at+1] != value {
					var found string
					if at+1 < len(got) {
						found = got[at+1]
					}
					t.Errorf("versioninfo.json says %s=%q but the committed .syso says %s=%q -- run `make versioninfo`", key, value, key, found)
				}
			}
		})
	}
}

// TestCommittedResourceCarriesThePlaceholderVersion keeps a release build's
// byproduct out of the repository. `make dist-windows-installer` regenerates
// these three files stamped with the real version -- deliberately -- and the
// working tree is left dirty. Committing that would pin every later build to
// one release's number in the file, which is a slower-moving version of the
// defect #1209 reports.
func TestCommittedResourceCarriesThePlaceholderVersion(t *testing.T) {
	root := repoRoot(t)
	zero := versionQuad{}
	for _, name := range shippedWindowsPrograms(t) {
		t.Run(name, func(t *testing.T) {
			vi := readVersionInfoJSON(t, filepath.Join(root, "cmd", name, "versioninfo.json"))
			if vi.FixedFileInfo.FileVersion != zero || vi.FixedFileInfo.ProductVersion != zero {
				t.Errorf("committed FixedFileInfo is %s / %s, want 0.0.0.0 -- the build stamps this, versioninfo.json holds the placeholder",
					vi.FixedFileInfo.FileVersion, vi.FixedFileInfo.ProductVersion)
			}
			for _, key := range []string{"FileVersion", "ProductVersion"} {
				if got := vi.StringFileInfo[key]; got != placeholderVersion {
					t.Errorf("committed StringFileInfo.%s = %q, want %q", key, got, placeholderVersion)
				}
			}
			// And the generated file agrees, so a stamped .syso committed
			// beside an unstamped .json is caught too.
			for _, s := range resourceStrings(t, filepath.Join(root, "cmd", name, "resource_windows_amd64.syso")) {
				if strings.HasPrefix(s, "0.0.") && s != placeholderVersion && s != "0.0.0.0" {
					t.Errorf("the committed .syso carries version string %q -- that looks like a `make dist-windows-installer` byproduct; run `make versioninfo` before committing", s)
				}
			}
		})
	}
}

// TestEveryWindowsArchHasAResource is the arm64 hole #1209 names. The .syso
// filename suffix is a build constraint: resource_windows_amd64.syso is
// linked into windows/amd64 and NOTHING else. A windows/arm64 target added
// later would silently produce nameless, 0.0.0.0 binaries again -- the exact
// starting condition -- so this fails on the day that target appears.
func TestEveryWindowsArchHasAResource(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, makefileRel))
	if err != nil {
		t.Fatalf("read %s: %v", makefileRel, err)
	}
	// GOOS=windows GOARCH=<arch> on one line, which is how every Windows
	// build line in the Makefile is written.
	re := regexp.MustCompile(`GOOS=windows\s+GOARCH=(\w+)`)
	found := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		found[m[1]] = true
	}
	if len(found) == 0 {
		t.Fatalf("no `GOOS=windows GOARCH=…` build lines found in %s -- the build moved and this guard now checks nothing", makefileRel)
	}
	for _, name := range shippedWindowsPrograms(t) {
		for arch := range found {
			rel := filepath.Join("cmd", name, "resource_windows_"+arch+".syso")
			if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
				t.Errorf("the Makefile builds %s for windows/%s, but %s is missing: a .syso is linked only into the GOARCH in its filename, so that build would ship with no version resource",
					name, arch, filepath.ToSlash(rel))
			}
		}
	}
}

// TestGeneratorPinIsTheSameEverywhere. The pin lives once in the Makefile so
// Renovate can see it (renovate.json's customManager), but the Smart App
// Control control-matrix script runs the same generator on a Windows host and
// says in its own comment that it does so "so this row measures the resource
// our build could actually ship". That claim is only true while the two
// versions agree.
func TestGeneratorPinIsTheSameEverywhere(t *testing.T) {
	root := repoRoot(t)
	re := regexp.MustCompile(`github\.com/josephspurrier/goversioninfo/cmd/goversioninfo@(v[\w.\-+]+)`)
	pins := map[string]string{}
	for _, rel := range []string{makefileRel, sacMatrixRel} {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		m := re.FindStringSubmatch(string(b))
		if m == nil {
			t.Fatalf("%s no longer pins goversioninfo by version -- this guard now checks nothing", rel)
		}
		pins[rel] = m[1]
	}
	if pins[makefileRel] != pins[sacMatrixRel] {
		t.Errorf("goversioninfo pinned at %s in %s but %s in %s", pins[makefileRel], makefileRel, pins[sacMatrixRel], sacMatrixRel)
	}
}

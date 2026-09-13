package controlclient

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every capability constant proto/signer publishes is either declared
// unconditionally by this build or listed below with a reason.
//
// The hand-written expectations in network_map_capability_test.go say
// what a declaration SHOULD contain; they cannot say that a constant was
// forgotten, because a constant nobody mentions is absent from both the
// production list and the test's. That is exactly how
// signer.CapabilityMeshShareV1 shipped undeclared (waired#1297): the
// control plane then never folded InferenceState.DesiredShare, so the
// console's mesh-sharing switch stored a value the device never heard —
// and the device kept reporting the setting "on" from its own boot
// default, which is indistinguishable from having been told so. It was
// found on real hardware.
//
// So this reads the constants from the source instead. A new capability
// fails here until someone decides, in writing, which side of the line
// it is on.
var capabilityNotDeclared = map[string]string{
	// The onboarding quartet is declared all-or-none and only by an
	// agent that has a setup reconciler, so it is not in the
	// unconditional list — declareCapabilities appends it when
	// OnboardingCapable. network_map_capability_test.go covers both
	// rows.
	"CapabilityOnboardingV1": "conditional: appended when OnboardingCapable",
	"CapabilityOnboardingV2": "conditional: appended when OnboardingCapable",
	"CapabilityOnboardingV3": "conditional: appended when OnboardingCapable",
	"CapabilityOnboardingV4": "conditional: appended when OnboardingCapable",
}

// unconditionalCapabilities returns the names in the `caps := []string{…}`
// literal, and only there.
//
// Parsed rather than grepped, which closes two holes the text match had
// (waired#1305). A capability appended inside the OnboardingCapable
// branch used to satisfy the check although it is undeclared on every
// non-wizard agent — the near neighbour of the failure that shipped. And
// a capability named only in a comment used to satisfy it too, in a file
// whose comments name most of them.
func unconditionalCapabilities(t *testing.T) map[string]bool {
	t.Helper()
	const src = "network_map.go"
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	got := map[string]bool{}
	var lit *ast.CompositeLit
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Tok != token.DEFINE || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); !ok || id.Name != "caps" {
			return true
		}
		if cl, ok := as.Rhs[0].(*ast.CompositeLit); ok {
			lit = cl
		}
		return true
	})
	if lit == nil {
		t.Fatalf("no `caps := []string{…}` literal in %s — has the declaration moved?", src)
	}
	for _, e := range lit.Elts {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "signer" {
			got[sel.Sel.Name] = true
		}
	}
	return got
}

func TestEveryProtoCapabilityIsDecided(t *testing.T) {
	const capabilitySrc = "../../proto/signer/capability.go"
	src, err := os.ReadFile(capabilitySrc)
	if err != nil {
		t.Fatalf("read %s: %v", capabilitySrc, err)
	}
	names := regexp.MustCompile(`(?m)^\s*(Capability\w+)\s*=\s*"`).FindAllStringSubmatch(string(src), -1)

	// A floor, not a count. Without it a moved file or a changed spelling
	// would leave the regex matching nothing and this test passing
	// vacuously — quiet in exactly the situation it exists for.
	if len(names) < 8 {
		t.Fatalf("found %d capability constants in %s, want at least 8 — has the file moved?",
			len(names), capabilitySrc)
	}

	declared := unconditionalCapabilities(t)
	// A floor well below the real count, on the other file: this is here
	// to catch a parse that found the wrong `caps`, not to count. Setting
	// it at the current number would make every ordinary removal fail
	// with the wrong message, and the per-name check below is the one
	// with something to say.
	if len(declared) < 4 {
		t.Fatalf("found %d unconditional capabilities in network_map.go, want at least 4 — "+
			"did the parse find the wrong literal?", len(declared))
	}

	for _, m := range names {
		name := m[1]
		if reason, ok := capabilityNotDeclared[name]; ok {
			if reason == "" {
				t.Errorf("%s is excluded with no reason", name)
			}
			if declared[name] {
				t.Errorf("%s is listed as not unconditionally declared, but the caps literal declares it", name)
			}
			continue
		}
		if !declared[name] {
			t.Errorf("%s is published by proto/signer but this build neither declares it "+
				"unconditionally nor lists it in capabilityNotDeclared. An undeclared "+
				"capability means the control plane never sends the field it gates, silently.", name)
		}
	}

	// And the other direction: an exclusion that matches no constant is a
	// stale claim, and nothing was reading it.
	found := map[string]bool{}
	for _, m := range names {
		found[m[1]] = true
	}
	for name := range capabilityNotDeclared {
		if !found[name] {
			t.Errorf("capabilityNotDeclared lists %s, which proto/signer no longer publishes", name)
		}
	}
}

// TestCapabilityCSVFitsTheColumn is the other half of the same worry.
//
// The control plane stores the declared set in a STRING(256) column and
// its normalizer sorts, then drops whatever does not fit, from the tail.
// Nothing about that order relates to what matters, and until waired#1303
// it happened in silence — a gated field simply stopping arriving, with
// nothing anywhere saying why. That is the shape waired#1297 shipped and
// real hardware had to find.
//
// The list is written here, so the headroom is the agent's to keep. The
// margin makes this fail while there is still room to think, rather than
// on the commit that overflows.
func TestCapabilityCSVFitsTheColumn(t *testing.T) {
	const (
		columnBytes = 256
		// Enough for two more capabilities of ordinary length.
		wantSpare = 32
	)
	const capabilitySrc = "../../proto/signer/capability.go"
	src, err := os.ReadFile(capabilitySrc)
	if err != nil {
		t.Fatalf("read %s: %v", capabilitySrc, err)
	}
	value := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(Capability\w+)\s*=\s*"([^"]+)"`).FindAllStringSubmatch(string(src), -1) {
		value[m[1]] = m[2]
	}

	// Worst case: a wizard-driven install, which appends the onboarding
	// quartet to the same CSV.
	var toks []string
	for name := range unconditionalCapabilities(t) {
		toks = append(toks, value[name])
	}
	for name := range capabilityNotDeclared {
		toks = append(toks, value[name])
	}
	if len(toks) < 8 {
		t.Fatalf("resolved %d capability values, want at least 8 — did the constant spelling change?", len(toks))
	}
	sort.Strings(toks)
	csv := strings.Join(toks, ",")
	if len(csv) > columnBytes-wantSpare {
		t.Fatalf("the declared capability CSV is %d bytes of a %d-byte column, leaving less than %d spare:\n%s\n"+
			"tokens past the limit are dropped from the tail, so a capability stops being folded with no error",
			len(csv), columnBytes, wantSpare, csv)
	}
	t.Logf("capability CSV: %d/%d bytes across %d capabilities", len(csv), columnBytes, len(toks))
}

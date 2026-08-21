package router

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// rankTier is the one bit the probe layer gets about the Selector's ranking
// (waired-agent#880): same tier means every key tied and only the arbitrary
// deviceID suffix separated them.

func TestAssignRankTiers(t *testing.T) {
	// Already in sorted order, as assignRankTiers requires.
	cands := []meshCandidate{
		{deviceID: "a", priority: 2},
		{deviceID: "b", priority: 2},
		{deviceID: "c", priority: 1},
		{deviceID: "d", priority: 1},
		{deviceID: "e", priority: 1, loadFraction: 0.5},
	}
	assignRankTiers(cands)

	want := []int{0, 0, 1, 1, 2}
	for i, c := range cands {
		if c.rankTier != want[i] {
			t.Errorf("%s: tier = %d, want %d", c.deviceID, c.rankTier, want[i])
		}
	}
}

func TestAssignRankTiers_EmptyAndSingle(t *testing.T) {
	assignRankTiers(nil) // must not panic
	one := []meshCandidate{{deviceID: "a"}}
	assignRankTiers(one)
	if one[0].rankTier != 0 {
		t.Errorf("a lone candidate is tier %d, want 0", one[0].rankTier)
	}
}

// TestSameRankExceptDeviceID_IgnoresOnlyTheSuffix: every ranking key must
// separate tiers, and deviceID must not.
func TestSameRankExceptDeviceID_IgnoresOnlyTheSuffix(t *testing.T) {
	base := meshCandidate{
		deviceID: "a", public: false, silent: false,
		priority: 1, score: 100, errorRate: 0, rttMS: 10, loadFraction: 0.25,
	}
	if !sameRankExceptDeviceID(base, func() meshCandidate { c := base; c.deviceID = "z"; return c }()) {
		t.Error("deviceID separated two candidates; it is the arbitrary suffix this exists to ignore")
	}
	for _, tc := range []struct {
		name string
		mut  func(*meshCandidate)
	}{
		{"public", func(c *meshCandidate) { c.public = true }},
		{"silent", func(c *meshCandidate) { c.silent = true }},
		{"priority", func(c *meshCandidate) { c.priority = 2 }},
		{"score", func(c *meshCandidate) { c.score = 200 }},
		{"errorRate", func(c *meshCandidate) { c.errorRate = 0.5 }},
		{"rttBucket", func(c *meshCandidate) { c.rttMS = 900 }},
		{"loadFraction", func(c *meshCandidate) { c.loadFraction = 0.75 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := base
			tc.mut(&other)
			if sameRankExceptDeviceID(base, other) {
				t.Errorf("%s did not separate two candidates, so a peer that outranks "+
					"on it could be displaced by a tie-break", tc.name)
			}
		})
	}
}

// TestSameRankExceptDeviceID_ListsEverySortKey is the guard that matters
// long-term: a key added to sortMeshCandidates and not here would silently
// widen a "tie" to cover candidates the Selector actually ranked apart, and
// residency would start overturning rather than breaking ties. Nothing in the
// compiler notices that, so this reads both lists.
//
// A record of today's behaviour, not a product contract: it pins that the two
// functions name the same fields, not what those fields should be.
func TestSameRankExceptDeviceID_ListsEverySortKey(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "endpoint_router.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sortKeys := fieldsRead(t, f, "sortMeshCandidates")
	tierKeys := fieldsRead(t, f, "sameRankExceptDeviceID")

	// deviceID is the suffix the tier predicate exists to ignore.
	delete(sortKeys, "deviceID")
	for k := range sortKeys {
		if !tierKeys[k] {
			t.Errorf("sortMeshCandidates ranks on %q and sameRankExceptDeviceID does not read it: "+
				"candidates the Selector ranked apart would land in one tier, and the "+
				"waired-agent#880 tie-break would overturn that key instead of breaking a tie", k)
		}
	}
	for k := range tierKeys {
		if !sortKeys[k] && k != "rttMS" {
			t.Errorf("sameRankExceptDeviceID reads %q and sortMeshCandidates does not rank on it: "+
				"tiers would split candidates the Selector considers identical", k)
		}
	}
}

// fieldsRead collects the meshCandidate field names a function selects on.
// Approximate by construction — it reads selector expressions, so a key
// derived some other way is invisible to it — which is why the test above
// says what it pins.
func fieldsRead(t *testing.T, f *ast.File, fn string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	var decl *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn {
			decl = fd
			break
		}
	}
	if decl == nil {
		t.Fatalf("%s not found in endpoint_router.go", fn)
	}
	ast.Inspect(decl, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := sel.Sel.Name
		// Exported names are methods and other packages' symbols, not
		// meshCandidate fields.
		if name != "" && strings.ToLower(name[:1]) == name[:1] {
			out[name] = true
		}
		return true
	})
	return out
}

// TestSortMeshCandidates_AssignsTiers: the tier is filled by the sort, so no
// caller has to remember to, and it is meaningless on an unsorted slice.
func TestSortMeshCandidates_AssignsTiers(t *testing.T) {
	cands := []meshCandidate{
		{deviceID: "low", priority: 1},
		{deviceID: "high", priority: 3},
		{deviceID: "high2", priority: 3},
	}
	sortMeshCandidates(cands)
	if cands[0].rankTier != cands[1].rankTier {
		t.Errorf("the two equally-ranked peers are in tiers %d and %d",
			cands[0].rankTier, cands[1].rankTier)
	}
	if cands[2].rankTier == cands[0].rankTier {
		t.Errorf("a lower-priority peer shares tier %d with the leaders", cands[2].rankTier)
	}
}

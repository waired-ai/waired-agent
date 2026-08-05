package catalog

import (
	"strings"
	"testing"
)

// TestCheckNameUniqueness is the A/B for a guard that had no equivalent
// until #521 moved a dozen aliases in one commit: a name claimed by two
// manifests resolves to whichever file sorts first, silently. The
// shipped catalog passing is asserted in the same test, so the guard
// cannot be satisfied by a rule nothing can meet.
func TestCheckNameUniqueness(t *testing.T) {
	bundled, err := BundledManifestsIncludingInternal()
	if err != nil {
		t.Fatalf("BundledManifestsIncludingInternal: %v", err)
	}
	if err := CheckNameUniqueness(bundled); err != nil {
		t.Errorf("the shipped catalog has a duplicate name: %v", err)
	}

	cases := []struct {
		name      string
		manifests []Manifest
		wantErr   string
	}{
		{
			// The failure mode itself: two manifests claiming one
			// alias. Before #521 this shipped without a diagnostic.
			name: "two manifests claim one alias",
			manifests: []Manifest{
				{ModelID: "a", ModelAliases: []string{"shared/name"}},
				{ModelID: "b", ModelAliases: []string{"shared/name"}},
			},
			wantErr: "shared/name",
		},
		{
			// One manifest's alias colliding with another's id is
			// the same hazard wearing a different hat, and worse:
			// LookupByAlias checks ModelID first, so the alias
			// loses to a model that never meant to take it.
			name: "an alias collides with another manifest's id",
			manifests: []Manifest{
				{ModelID: "a"},
				{ModelID: "b", ModelAliases: []string{"a"}},
			},
			wantErr: `"a"`,
		},
		{
			// Not a collision: most of the bundled set repeats its
			// own id in model_aliases, and LookupByAlias checks
			// ModelID first anyway. A guard that failed here would
			// be a guard nobody could turn on.
			name: "a manifest may repeat its own id as an alias",
			manifests: []Manifest{
				{ModelID: "a", ModelAliases: []string{"a", "vendor/a"}},
				{ModelID: "b", ModelAliases: []string{"b"}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckNameUniqueness(tc.manifests)
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("CheckNameUniqueness = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckNameUniqueness = nil, want an error naming %s", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not name %s", err, tc.wantErr)
			}
		})
	}
}

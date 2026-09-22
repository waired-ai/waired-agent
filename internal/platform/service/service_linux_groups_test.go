//go:build linux

package service

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// PRODUCT CONTRACT (#1535, owner decision 2026-09-22): the service user
// joins `render`, where the host has it, and never `video`. The engine
// runs as that user and cannot open an AMD or Intel GPU without the
// render group; `video` would add cameras and the display for nothing
// computing needs.
func TestEnsureGPUGroups(t *testing.T) {
	joinErr := errors.New("cannot lock /etc/group")
	for _, tc := range []struct {
		name      string
		groups    map[string]bool // what the group database knows
		joinErr   error
		wantAsked []string
		wantJoins []string // "user:group"
		wantErr   string
	}{
		{
			name:      "render exists: the user joins it",
			groups:    map[string]bool{"render": true, "video": true},
			wantAsked: []string{"render"},
			wantJoins: []string{"waired:render"},
		},
		{
			name:      "no render group: nothing is joined, nothing is created",
			groups:    map[string]bool{"video": true},
			wantAsked: []string{"render"},
		},
		{
			name:      "a failed join fails the install, naming the group",
			groups:    map[string]bool{"render": true},
			joinErr:   joinErr,
			wantAsked: []string{"render"},
			wantJoins: []string{"waired:render"},
			wantErr:   `add user "waired" to group "render"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked, joins []string
			exists := func(g string) bool {
				asked = append(asked, g)
				return tc.groups[g]
			}
			join := func(user, group string) error {
				joins = append(joins, user+":"+group)
				return tc.joinErr
			}
			err := ensureGPUGroups("waired", exists, join)
			if tc.wantErr == "" && err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !errors.Is(err, joinErr) {
					t.Fatalf("err = %v, want it to contain %q and wrap the join error", err, tc.wantErr)
				}
			}
			if !reflect.DeepEqual(asked, tc.wantAsked) {
				t.Errorf("asked about %v, want %v", asked, tc.wantAsked)
			}
			if !reflect.DeepEqual(joins, tc.wantJoins) {
				t.Errorf("joined %v, want %v", joins, tc.wantJoins)
			}
		})
	}
}

// groupExists is the real lookup ensureGPUGroups gets from Install. Group
// "root" (gid 0) is in every Linux group database.
func TestGroupExists(t *testing.T) {
	if !groupExists("root") {
		t.Error(`groupExists("root") = false, want true`)
	}
	if groupExists("waired-no-such-group-1535") {
		t.Error("groupExists(absent) = true, want false")
	}
}

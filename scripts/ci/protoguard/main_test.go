package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProto lays out a fake proto tree: root files go into signer/.
func writeProto(t *testing.T, src string) string {
	t.Helper()
	dir := t.TempDir()
	pkgDir := filepath.Join(dir, "signer")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "signer.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const oldSrc = `package signer

const CapabilityFooV1 = "foo-v1"

type Peer struct {
	DeviceID string ` + "`json:\"device_id\"`" + `
	Extra    string ` + "`json:\"extra,omitempty\"`" + `
}

func Verify(msg []byte) error { return nil }
`

func mustRun(t *testing.T, oldDir, newDir string) []string {
	t.Helper()
	violations, err := run(oldDir, newDir)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return violations
}

func assertViolation(t *testing.T, violations []string, want string) {
	t.Helper()
	for _, v := range violations {
		if strings.Contains(v, want) {
			return
		}
	}
	t.Fatalf("violations %v missing %q", violations, want)
}

func TestIdenticalAPI_Passes(t *testing.T) {
	if v := mustRun(t, writeProto(t, oldSrc), writeProto(t, oldSrc)); len(v) != 0 {
		t.Fatalf("identical trees should pass, got %v", v)
	}
}

func TestAdditiveChanges_Pass(t *testing.T) {
	newSrc := `package signer

const CapabilityFooV1 = "foo-v1"
const CapabilityBarV1 = "bar-v1"

type Peer struct {
	DeviceID string ` + "`json:\"device_id\"`" + `
	Extra    string ` + "`json:\"extra,omitempty\"`" + `
	Grant    *Grant ` + "`json:\"grant,omitempty\"`" + `
	internal string
}

type Grant struct {
	ID string ` + "`json:\"id\"`" + `
}

func Verify(msg []byte) error { return nil }
func Sign(msg []byte) []byte  { return nil }
`
	if v := mustRun(t, writeProto(t, oldSrc), writeProto(t, newSrc)); len(v) != 0 {
		t.Fatalf("additive changes should pass, got %v", v)
	}
}

func TestNonAdditiveChanges_Fail(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{
			name: "field removed",
			src: `package signer
const CapabilityFooV1 = "foo-v1"
type Peer struct {
	DeviceID string ` + "`json:\"device_id\"`" + `
}
func Verify(msg []byte) error { return nil }
`,
			want: "Peer.Extra: field removed",
		},
		{
			name: "json tag changed",
			src: `package signer
const CapabilityFooV1 = "foo-v1"
type Peer struct {
	DeviceID string ` + "`json:\"device\"`" + `
	Extra    string ` + "`json:\"extra,omitempty\"`" + `
}
func Verify(msg []byte) error { return nil }
`,
			want: "Peer.DeviceID: struct tag changed",
		},
		{
			name: "field type changed",
			src: `package signer
const CapabilityFooV1 = "foo-v1"
type Peer struct {
	DeviceID int    ` + "`json:\"device_id\"`" + `
	Extra    string ` + "`json:\"extra,omitempty\"`" + `
}
func Verify(msg []byte) error { return nil }
`,
			want: "Peer.DeviceID: type changed",
		},
		{
			name: "added field without omitempty",
			src: `package signer
const CapabilityFooV1 = "foo-v1"
type Peer struct {
	DeviceID string ` + "`json:\"device_id\"`" + `
	Extra    string ` + "`json:\"extra,omitempty\"`" + `
	Loud     string ` + "`json:\"loud\"`" + `
}
func Verify(msg []byte) error { return nil }
`,
			want: "Peer.Loud: field added to a published struct without omitempty",
		},
		{
			name: "const value changed",
			src: `package signer
const CapabilityFooV1 = "foo-v2"
type Peer struct {
	DeviceID string ` + "`json:\"device_id\"`" + `
	Extra    string ` + "`json:\"extra,omitempty\"`" + `
}
func Verify(msg []byte) error { return nil }
`,
			want: "CapabilityFooV1: const value changed",
		},
		{
			name: "const removed",
			src: `package signer
type Peer struct {
	DeviceID string ` + "`json:\"device_id\"`" + `
	Extra    string ` + "`json:\"extra,omitempty\"`" + `
}
func Verify(msg []byte) error { return nil }
`,
			want: "CapabilityFooV1: const removed",
		},
		{
			name: "func signature changed",
			src: `package signer
const CapabilityFooV1 = "foo-v1"
type Peer struct {
	DeviceID string ` + "`json:\"device_id\"`" + `
	Extra    string ` + "`json:\"extra,omitempty\"`" + `
}
func Verify(msg []byte, strict bool) error { return nil }
`,
			want: "Verify: signature changed",
		},
		{
			name: "struct removed",
			src: `package signer
const CapabilityFooV1 = "foo-v1"
func Verify(msg []byte) error { return nil }
`,
			want: "Peer: struct removed",
		},
	}
	oldDir := writeProto(t, oldSrc)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			violations := mustRun(t, oldDir, writeProto(t, c.src))
			assertViolation(t, violations, c.want)
		})
	}
}

// A parameter rename is not an API change: `func(_ Host)` and
// `func(h Host)` are the same signature to every caller. Grouped and
// ungrouped names are the same list, too. A changed type still fails.
func TestParameterRename_Passes(t *testing.T) {
	renamed := strings.Replace(oldSrc, "func Verify(msg []byte) error", "func Verify(_ []byte) (err error)", 1)
	if v := mustRun(t, writeProto(t, oldSrc), writeProto(t, renamed)); len(v) != 0 {
		t.Fatalf("a parameter rename should pass, got %v", v)
	}
	grouped := `package signer
func Pair(a, b int) int { return a }
`
	ungrouped := `package signer
func Pair(x int, y int) int { return x }
`
	if v := mustRun(t, writeProto(t, grouped), writeProto(t, ungrouped)); len(v) != 0 {
		t.Fatalf("regrouping named parameters should pass, got %v", v)
	}
	retyped := `package signer
func Pair(x int, y int64) int { return x }
`
	assertViolation(t, mustRun(t, writeProto(t, grouped), writeProto(t, retyped)), "Pair: signature changed")
}

func TestAddedFieldJSONDashIsSafe(t *testing.T) {
	newSrc := `package signer
const CapabilityFooV1 = "foo-v1"
type Peer struct {
	DeviceID string ` + "`json:\"device_id\"`" + `
	Extra    string ` + "`json:\"extra,omitempty\"`" + `
	Skip     string ` + "`json:\"-\"`" + `
}
func Verify(msg []byte) error { return nil }
`
	if v := mustRun(t, writeProto(t, oldSrc), writeProto(t, newSrc)); len(v) != 0 {
		t.Fatalf("json:\"-\" addition should pass, got %v", v)
	}
}

func TestRemovedPackage_Fails(t *testing.T) {
	empty := t.TempDir()
	violations := mustRun(t, writeProto(t, oldSrc), empty)
	assertViolation(t, violations, "package signer: removed")
}

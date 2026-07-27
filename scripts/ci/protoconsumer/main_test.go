package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/waired-ai/waired-agent/proto/signer"
)

func runFixture(t *testing.T, dir string, exempt map[fieldKey]claim) []string {
	t.Helper()
	if exempt == nil {
		exempt = map[fieldKey]claim{}
	}
	got, _, err := guard(dir+"/proto", []string{dir + "/cmd"}, exempt)
	if err != nil {
		t.Fatalf("guard(%s): %v", dir, err)
	}
	sort.Strings(got)
	return got
}

func mustContain(t *testing.T, got []string, want string) {
	t.Helper()
	for _, g := range got {
		if strings.Contains(g, want) {
			return
		}
	}
	t.Errorf("no violation mentioning %q; got:\n  %s", want, strings.Join(got, "\n  "))
}

func mustNotContain(t *testing.T, got []string, unwanted string) {
	t.Helper()
	for _, g := range got {
		if strings.Contains(g, unwanted) {
			t.Errorf("unexpected violation mentioning %q: %s", unwanted, g)
		}
	}
}

// The acceptance case for #207: the host-fit fields as they stood before
// the producer was re-landed. A consumer exists on the control-plane
// side, the wire is byte-identical because every added field is
// omitempty, and no test in this repo goes red — except this guard.
func TestGuard_HostFitFieldsWithoutProducerFail(t *testing.T) {
	got := runFixture(t, "testdata/hostfit", nil)

	mustContain(t, got, "hardware.HardwareSummary.UnifiedMemory: no producer")
	mustContain(t, got, "hardware.HardwareSummary.UsableVRAMMB: no producer")
	mustContain(t, got, "hardware.GPU.Vendor: no producer")

	// Written by non-test code, so not a violation.
	mustNotContain(t, got, "Hostname")
	mustNotContain(t, got, "GPU.Model")
	// Unexported fields are not part of the wire contract.
	mustNotContain(t, got, "unexported")
	// A type alias is not a struct.
	mustNotContain(t, got, "notAStruct")

	if len(got) != 3 {
		t.Errorf("want exactly 3 violations, got %d:\n  %s", len(got), strings.Join(got, "\n  "))
	}
}

// Re-landing the producer clears the guard with no table entry at all —
// writing the field is the primary fix, exempting it the fallback.
func TestGuard_ProducerRelandedPasses(t *testing.T) {
	if got := runFixture(t, "testdata/hostfit-fixed", nil); len(got) != 0 {
		t.Errorf("want clean, got:\n  %s", strings.Join(got, "\n  "))
	}
}

// A field written only from _test.go is still unproduced: that is the
// state a green test suite hid for #180.
func TestGuard_TestOnlyWriterDoesNotCount(t *testing.T) {
	got := runFixture(t, "testdata/hostfit", nil)
	mustContain(t, got, "UnifiedMemory")
}

func TestGuard_ExemptionSilencesTheField(t *testing.T) {
	exempt := map[fieldKey]claim{
		{"hardware", "HardwareSummary", "UnifiedMemory"}: {receiveOnlyKind, "receive-only"},
		{"hardware", "HardwareSummary", "UsableVRAMMB"}:  {receiveOnlyKind, "receive-only"},
		{"hardware", "GPU", "Vendor"}:                    {receiveOnlyKind, "receive-only"},
	}
	if got := runFixture(t, "testdata/hostfit", exempt); len(got) != 0 {
		t.Errorf("want clean, got:\n  %s", strings.Join(got, "\n  "))
	}
}

// Staleness, direction 1: the exemption outlived the field.
func TestGuard_ExemptionForMissingFieldFails(t *testing.T) {
	exempt := map[fieldKey]claim{
		{"hardware", "HardwareSummary", "UnifiedMemory"}: {receiveOnlyKind, "receive-only"},
		{"hardware", "HardwareSummary", "UsableVRAMMB"}:  {receiveOnlyKind, "receive-only"},
		{"hardware", "GPU", "Vendor"}:                    {receiveOnlyKind, "receive-only"},
		{"hardware", "GPU", "Removed"}:                   {receiveOnlyKind, "renamed away three releases ago"},
	}
	got := runFixture(t, "testdata/hostfit", exempt)
	mustContain(t, got, "hardware.GPU.Removed: listed as receiveOnly but no such exported proto field exists")
}

// Staleness, direction 2: the producer landed and the entry stayed. The
// entry has to go, or the next field to lose its writer hides behind it.
func TestGuard_ExemptionForProducedFieldFails(t *testing.T) {
	exempt := map[fieldKey]claim{
		{"hardware", "HardwareSummary", "UnifiedMemory"}: {receiveOnlyKind, "no longer true"},
	}
	got := runFixture(t, "testdata/hostfit-fixed", exempt)
	mustContain(t, got, "listed as receiveOnly but something under testdata/hostfit-fixed/cmd/ now writes it")
}

// Every syntactic form a producer can take has to count, or the guard
// invents violations and everyone learns to exempt their way past it.
func TestGuard_RecognisesEveryWriteForm(t *testing.T) {
	if got := runFixture(t, "testdata/writeforms", nil); len(got) != 0 {
		t.Errorf("write forms not recognised:\n  %s", strings.Join(got, "\n  "))
	}
}

func TestResolveExemptions_Rejects(t *testing.T) {
	cases := []struct {
		name string
		in   []exemption
		want string
	}{
		{"nil struct", []exemption{{nil, "X", "why"}}, "nil Struct"},
		{"field gone", []exemption{{reflect.TypeFor[signer.SetupProgress](), "Nope", "why"}}, `has no field "Nope"`},
		{"outside proto", []exemption{{reflect.TypeFor[exemption](), "Field", "why"}}, "not in the proto module"},
		{"no reason", []exemption{{reflect.TypeFor[signer.SetupProgress](), "Driver", "  "}}, "has no reason"},
		{"duplicate", []exemption{
			{reflect.TypeFor[signer.SetupProgress](), "Driver", "why"},
			{reflect.TypeFor[signer.SetupProgress](), "Driver", "why again"},
		}, "duplicate exemption"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := resolveExemptions(table{receiveOnlyKind, tc.in})
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestResolveExemptions_KeyIsPackageRelative(t *testing.T) {
	got, err := resolveExemptions(table{producerPendingKind, []exemption{
		{reflect.TypeFor[signer.SetupProgress](), "Driver", "pending #198"},
	}})
	if err != nil {
		t.Fatalf("resolveExemptions: %v", err)
	}
	want := fieldKey{Pkg: "signer", Struct: "SetupProgress", Field: "Driver"}
	if _, ok := got[want]; !ok {
		t.Errorf("key %v missing; got %v", want, got)
	}
}

// A category is a claim about where the value comes from, so a wrong one
// has to fail as loudly as a missing entry. proto/hostfit is the case
// that forced this: it is a pure computation package, not a wire schema,
// and its result fields are written by proto itself.
func TestGuard_ProducedInProtoMustActuallyBeWrittenInProto(t *testing.T) {
	exempt := map[fieldKey]claim{
		{"hardware", "HardwareSummary", "UnifiedMemory"}: {producedInProtoKind, "claims proto writes it"},
		{"hardware", "HardwareSummary", "UsableVRAMMB"}:  {receiveOnlyKind, "receive-only"},
		{"hardware", "GPU", "Vendor"}:                    {receiveOnlyKind, "receive-only"},
	}
	got := runFixture(t, "testdata/hostfit", exempt)
	mustContain(t, got, "listed as producedInProto, but nothing under testdata/hostfit/proto/ writes it either")
}

func TestGuard_ProtoWrittenFieldMustNotClaimAnExternalProducer(t *testing.T) {
	exempt := map[fieldKey]claim{
		{"p", "W", "ByAssign"}: {receiveOnlyKind, "wrongly claims someone else writes it"},
	}
	got := runFixture(t, "testdata/protowriter", exempt)
	mustContain(t, got, "move the entry to producedInProto")
}

// The same fixture with the honest category is clean, and stays clean
// only because proto really does write the field.
func TestGuard_ProducedInProtoPasses(t *testing.T) {
	exempt := map[fieldKey]claim{
		{"p", "W", "ByAssign"}: {producedInProtoKind, "filled by the package's own constructor"},
	}
	if got := runFixture(t, "testdata/protowriter", exempt); len(got) != 0 {
		t.Errorf("want clean, got:\n  %s", strings.Join(got, "\n  "))
	}
}

// The declared tables describe this repository as it is right now. Kept
// here as well as in the lint job so `go test ./...` catches a stale
// entry — the lint step and this test fail for the same reason, which is
// the point: whichever you run first tells you.
func TestRepoTablesAreCurrent(t *testing.T) {
	exempt, err := repoExemptions()
	if err != nil {
		t.Fatalf("resolveExemptions: %v", err)
	}
	const root = "../../.."
	got, st, err := guard(root+"/proto", []string{root + "/cmd", root + "/internal"}, exempt)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("guard is not clean against the repo:\n  %s", strings.Join(got, "\n  "))
	}
	if st.fields == 0 || st.produced == 0 {
		t.Fatalf("scan found nothing (%+v) — the fixture paths are probably wrong", st)
	}
}

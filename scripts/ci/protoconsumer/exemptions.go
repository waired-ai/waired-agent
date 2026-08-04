package main

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/waired-ai/waired-agent/proto/catalog"
	"github.com/waired-ai/waired-agent/proto/disco"
	"github.com/waired-ai/waired-agent/proto/frame"
	"github.com/waired-ai/waired-agent/proto/hostfit"
	"github.com/waired-ai/waired-agent/proto/signer"
)

// This file is the single source of truth for proto fields that have no
// producer under cmd/ or internal/. Nothing else — no inline comment, no
// side ledger — clears the guard, so the whole set is readable in one
// place.
//
// Three reasons a field legitimately has no producer there, and the
// category is part of the claim, not decoration — guard() checks each
// one against the source:
//
//	receiveOnly     — someone else writes it. The control plane, a relay,
//	                  a peer, or the catalog authoring pipeline. This
//	                  repo decodes it and reads it. Permanent.
//	producedInProto — the proto module writes it itself: a constructor, a
//	                  decoder, or a pure computation whose result type
//	                  lives beside it. Not every proto package is a wire
//	                  schema. Permanent.
//	producerPending — this repo is SUPPOSED to write it and does not yet.
//	                  The wire landed ahead of the producer, which is the
//	                  exact shape of #180 — so each entry names the issue
//	                  that owes the writer, and the entry is deleted (not
//	                  edited) when that issue lands.

// receiveOnly: written elsewhere, decoded here.
var receiveOnly = []exemption{
	// Bundled catalog manifests (proto/catalog/bundled/*.json) are
	// authored upstream and decoded by the agent. Nothing in this repo
	// ever writes a manifest, so every manifest field is receive-only;
	// these are the ones no code happens to construct in a test fixture.
	{reflect.TypeFor[catalog.Security](), "AllowPersistentKVCache",
		"bundled catalog manifest field; authored upstream, decoded here"},
	{reflect.TypeFor[catalog.Security](), "TrustRemoteCodeRequired",
		"bundled catalog manifest field; authored upstream, decoded here"},
	{reflect.TypeFor[catalog.VariantSource](), "Tag",
		"bundled catalog manifest field; authored upstream, decoded here"},
	{reflect.TypeFor[catalog.Manifest](), "InternalOnly",
		"withholds a shipped model from every offer surface; authored in the manifest, read by BundledManifests"},
	{reflect.TypeFor[catalog.VendorRuntimeSupport](), "LlamaCPP",
		"vendor×runtime support cell; authored in the catalog, read by the picker"},
	{reflect.TypeFor[catalog.VendorRuntimeSupport](), "MLX",
		"vendor×runtime support cell; authored in the catalog, read by the picker"},
	{reflect.TypeFor[catalog.VendorSupportMatrix](), "Nvidia",
		"vendor×runtime support cell; authored in the catalog, read by the picker"},
	{reflect.TypeFor[catalog.VendorSupportMatrix](), "AMD",
		"vendor×runtime support cell; authored in the catalog, read by the picker"},
	{reflect.TypeFor[catalog.VendorSupportMatrix](), "Mac",
		"vendor×runtime support cell; authored in the catalog, read by the picker"},

	// relay → agent handshake and data frames. The relay lives in the
	// private monorepo; the agent only ever decodes these.
	{reflect.TypeFor[frame.EncryptedPacket](), "PacketID",
		"relay→agent data frame; the relay writes it, the agent decodes"},
	{reflect.TypeFor[frame.RelayChallenge](), "RelayID",
		"relay→agent handshake challenge; the relay writes it, the agent decodes"},
	{reflect.TypeFor[frame.RelayChallenge](), "RelayNonce",
		"relay→agent handshake challenge; the relay writes it, the agent decodes"},
	{reflect.TypeFor[frame.RelayChallenge](), "ServerTime",
		"relay→agent handshake challenge; the relay writes it, the agent decodes"},
	{reflect.TypeFor[frame.RelayEstablished](), "RelaySessionID",
		"relay→agent session confirmation; the relay writes it, the agent decodes"},
	{reflect.TypeFor[frame.RelayEstablished](), "MaxFrameSizeBytes",
		"relay→agent session confirmation; the relay writes it, the agent decodes"},
	{reflect.TypeFor[frame.RelayEstablished](), "HeartbeatIntervalSeconds",
		"relay→agent session confirmation; the relay writes it, the agent decodes"},

	// Control plane → agent: the signed network map and everything the
	// CP injects into it. The agent verifies and reads; writing any of
	// these here would break the signature.
	{reflect.TypeFor[signer.NetworkMap](), "MapEpoch",
		"CP-assembled signed network map; the agent verifies and reads it"},
	{reflect.TypeFor[signer.NetworkMap](), "Relays",
		"CP-assembled signed network map; the agent verifies and reads it"},
	{reflect.TypeFor[signer.NetworkMap](), "ActiveTestScenario",
		"CP-assembled signed network map; the agent verifies and reads it"},
	{reflect.TypeFor[signer.NetworkMapPeer](), "OwnerEmail",
		"CP-assembled peer entry; the agent verifies and reads it"},
	{reflect.TypeFor[signer.NetworkMapPeer](), "HomeRelay",
		"CP-assembled peer entry; the agent verifies and reads it"},
	{reflect.TypeFor[signer.NetworkMapPeer](), "AllowedServices",
		"CP-assembled peer entry; the agent verifies and reads it"},
	{reflect.TypeFor[signer.NetworkMapPeer](), "PrevNodePublicKey",
		"CP-assembled peer entry; the agent verifies and reads it"},
	{reflect.TypeFor[signer.NetworkMapRelay](), "RelayID",
		"CP-assembled relay entry; the agent verifies and reads it"},
	{reflect.TypeFor[signer.NetworkMapRelay](), "Region",
		"CP-assembled relay entry; the agent verifies and reads it"},
	{reflect.TypeFor[signer.NetworkMapRelay](), "DiscoHosts",
		"CP-assembled relay entry; the agent verifies and reads it"},
	{reflect.TypeFor[signer.NetworkMapRelay](), "TLSFingerprint",
		"CP-assembled relay entry; the agent verifies and reads it"},
	{reflect.TypeFor[signer.DeviceCertificate](), "AllowedServices",
		"CP-issued device certificate; the agent verifies and reads it"},
	{reflect.TypeFor[signer.ActiveTestScenario](), "ScenarioID",
		"CP-injected testnet scenario; the agent reads it"},
	{reflect.TypeFor[signer.ActiveTestScenario](), "ExpectedNonce",
		"CP-injected testnet scenario; the agent reads it"},

	// InferenceState carries both directions in one struct: the agent
	// pushes its own state, and the CP injects operator intent into the
	// Self entry at map-assembly time (effectiveInferenceState). These
	// four are the injected half — the agent never sets them on its
	// own push, and doing so would be a bug, not a fix.
	{reflect.TypeFor[signer.InferenceState](), "ExcludeMain",
		"CP-injected at map assembly; the agent never sets it on its own push"},
	{reflect.TypeFor[signer.InferenceState](), "ExcludeSub",
		"CP-injected at map assembly; the agent never sets it on its own push"},
	{reflect.TypeFor[signer.InferenceState](), "DesiredParallel",
		"CP-injected admin override; the agent reads it to drive OLLAMA_NUM_PARALLEL"},
	{reflect.TypeFor[signer.InferenceState](), "PublicCapacity",
		"CP-injected Public Share budget for the Self entry; the agent reads it"},
	{reflect.TypeFor[signer.InferenceState](), "DesiredIntegrations",
		"CP-injected onboarding target (which coding agents to configure); the agent reads it"},
	{reflect.TypeFor[signer.InferenceState](), "DesiredModelGen",
		"CP-injected retry generation for the model download; the agent reads it to re-admit the pull"},
}

// producedInProto: the proto module writes it itself. Not every package
// under proto/ is a wire schema — hostfit is a pure computation whose
// argument and result types live beside it, and the disco codec fills
// its own parsed structs. guard() verifies the claim: an entry here
// whose field nothing under proto/ writes fails.
var producedInProto = []exemption{
	{reflect.TypeFor[hostfit.Verdict](), "NeedMB",
		"a fit result, written by the hostfit decision itself"},
	{reflect.TypeFor[hostfit.Verdict](), "HaveMB",
		"a fit result, written by the hostfit decision itself"},
	{reflect.TypeFor[hostfit.Verdict](), "Estimate",
		"a fit result, written by the hostfit decision itself"},
	{reflect.TypeFor[hostfit.Estimate](), "TokpsEstimate",
		"the roofline decode prediction, computed inside hostfit"},
	{reflect.TypeFor[hostfit.Estimate](), "Resident",
		"the roofline decode prediction, computed inside hostfit"},
	{reflect.TypeFor[hostfit.Estimate](), "ResidentShare",
		"the roofline decode prediction, computed inside hostfit"},
	{reflect.TypeFor[hostfit.Estimate](), "UpperBound",
		"the roofline decode prediction, computed inside hostfit"},
	// hostfit.Presentation is built ONLY by hostfit.Project /
	// NoVariantForEngine — deliberately, so there is one producer of the
	// shape the tray, the CLI and the setup wizard all render
	// (waired-agent#321). A consumer that assembled one field by field
	// would be the second implementation this package exists to prevent.
	//
	// Reason and QualityTier are absent from this list on purpose: the
	// guard matches producers by field NAME, and both names are written
	// by same-named fields on local structs elsewhere in the repo. Adding
	// them here would fail as "a field that has since gained a writer".
	{reflect.TypeFor[hostfit.Presentation](), "Runnable",
		"the shared fit projection, built by hostfit.Project"},
	{reflect.TypeFor[hostfit.Presentation](), "NeedMB",
		"the shared fit projection, built by hostfit.Project"},
	{reflect.TypeFor[hostfit.Presentation](), "HaveMB",
		"the shared fit projection, built by hostfit.Project"},
	{reflect.TypeFor[hostfit.Presentation](), "RequiredResidentMB",
		"the shared fit projection, built by hostfit.Project"},
	{reflect.TypeFor[hostfit.Presentation](), "NotRecommended",
		"the shared fit projection, built by hostfit.Project"},
	{reflect.TypeFor[hostfit.Presentation](), "NotRecommendedReason",
		"the shared fit projection, built by hostfit.Project"},
	{reflect.TypeFor[hostfit.Presentation](), "Speed",
		"the shared fit projection, built by hostfit.Project"},
	{reflect.TypeFor[hostfit.Presentation](), "EstimatedTokps",
		"the shared fit projection, built by hostfit.Project"},
	{reflect.TypeFor[disco.Frame](), "Ed25519Sig",
		"the peer↔peer signature; disco.Decode fills it on receipt"},
	{reflect.TypeFor[disco.SealedHeader](), "Vers",
		"plaintext prefix of a received sealed frame, filled by ParseSealedHeader"},
	{reflect.TypeFor[disco.SealedHeader](), "SrcNodeKey",
		"plaintext prefix of a received sealed frame, filled by ParseSealedHeader"},
	// catalog.Retirement is a table of compiled-in facts, not a wire
	// message: proto/catalog/retired.go is the only writer, by design.
	// The agent reads it to migrate a name and the control plane reads it
	// to refuse one; a consumer that assembled a row itself would be
	// asserting a retirement nobody recorded.
	//
	// Reason is absent here for the same cause as Presentation.Reason
	// above — the guard matches producers by field NAME, and other
	// structs in this repo write one.
	{reflect.TypeFor[catalog.Retirement](), "Names",
		"the retirement table, written only by proto/catalog/retired.go"},
	{reflect.TypeFor[catalog.Retirement](), "SuccessorModelID",
		"the retirement table, written only by proto/catalog/retired.go"},
}

// producerPending: this repo owes the writer. Each entry names the issue
// that lands it; delete the entry in that PR rather than editing it.
//
// The onboarding-v2 wire landed as its own additive proto PR (#196, as
// the workspace rules require) with every agent-side producer still open,
// and this table held the debt in the open until each one was paid —
// `rate_bps` by #197, `driver` and the benchmark trial fields by
// #198/#199, and the onboarding-v3 wire (#411) did the same for
// `SetupProgress.ModelGen`, paid by #136. waired#1030's `NotShared` (#417)
// took the debt on the same terms and #428 paid it. waired#1031's
// `ContextWindow` (#439) was the most recent, and #440 paid it.
// waired#1064's `ActiveModel` (#469) took the debt on the same terms and
// this PR pays it. Empty again, and that is the point.
//
// (waired#1064's sibling field, InferenceState.SubsystemState, never
// appeared here, for the reason Presentation.Reason is absent above:
// management.InferenceStatus already has a field of that name, and the
// guard matches producers by field NAME, so it read the local write as
// this one's producer. Listing it would have failed as "a field that has
// since gained a writer" before its writer existed.)
var producerPending = []exemption{}

// exemption declares one proto field with no producer under cmd/ or
// internal/. The struct is a reflect.Type rather than a string so the
// compiler anchors it: renaming or deleting the struct breaks the build
// here instead of leaving an entry that silently covers nothing. The
// field name is a string — Go has no expression for "a field" — and is
// checked at run time against the type, and again against the parsed
// proto source by guard().
type exemption struct {
	Struct reflect.Type
	Field  string
	Reason string
}

const protoModulePrefix = "github.com/waired-ai/waired-agent/proto/"

// repoExemptions is the whole declared set, categorised.
func repoExemptions() (map[fieldKey]claim, error) {
	return resolveExemptions(
		table{receiveOnlyKind, receiveOnly},
		table{producedInProtoKind, producedInProto},
		table{producerPendingKind, producerPending},
	)
}

type table struct {
	Kind    kind
	Entries []exemption
}

// resolveExemptions flattens the declared tables into the (pkg, struct,
// field) keys guard() works in, failing on any entry whose field the
// named struct does not actually have.
func resolveExemptions(lists ...table) (map[fieldKey]claim, error) {
	out := map[fieldKey]claim{}
	for _, list := range lists {
		for _, e := range list.Entries {
			if e.Struct == nil {
				return nil, fmt.Errorf("exemption for field %q has a nil Struct", e.Field)
			}
			pkgPath := e.Struct.PkgPath()
			if !strings.HasPrefix(pkgPath, protoModulePrefix) {
				return nil, fmt.Errorf("%s.%s is not in the proto module (%s)",
					e.Struct.Name(), e.Field, pkgPath)
			}
			if _, ok := e.Struct.FieldByName(e.Field); !ok {
				return nil, fmt.Errorf("%s has no field %q — it was renamed or removed; update the exemption",
					e.Struct.String(), e.Field)
			}
			if strings.TrimSpace(e.Reason) == "" {
				return nil, fmt.Errorf("%s.%s has no reason", e.Struct.Name(), e.Field)
			}
			key := fieldKey{
				Pkg:    strings.TrimPrefix(pkgPath, protoModulePrefix),
				Struct: e.Struct.Name(),
				Field:  e.Field,
			}
			if _, dup := out[key]; dup {
				return nil, fmt.Errorf("duplicate exemption for %s", key)
			}
			out[key] = claim{Kind: list.Kind, Reason: e.Reason}
		}
	}
	return out, nil
}

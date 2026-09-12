package inference

// Serving readiness — "may anything be told this node is ready?" — is
// defined once, here, and read by every surface that answers that
// question: the peer admission check in internal/router, the local
// status and doctor lines, the tray, and the subsystem state the mesh
// and the control plane see.
//
// It exists because the product had no such predicate. Readiness was
// spelled out separately at every surface, and every spelling answered
// "process alive + model file on disk" (the EngineReady doc above says
// so in as many words). Measured on a Strix Halo / Windows host
// (waired-agent#1307): the agent's own warm-up started loading the
// weights at 19:11:58Z, the host reported ready at 19:12:10Z with
// /api/ps still empty, and the first request arrived at 19:12:12Z and
// waited behind that load. Nothing was broken — every predicate
// answered the question it was written to answer. There just was no
// predicate for the question a requester is actually asking.
//
// Owner ruling, 2026-09-12: "the model is loaded and inference can be
// served" is a term of readiness in EVERY case, not only during setup.
// The scenarios are not hypothetical — an engine bounce for a tuning
// step-down, a cancelled model switch, a machine reboot and the
// host-speed probe's own eviction all leave a node reporting ready with
// nothing in memory.
//
// ServingTerms is a struct of facts rather than a bool because the
// reason is as load-bearing as the verdict: `waired status` renders it,
// the probe client puts it in X-Waired-Fallback-Reason, and a peer that
// is merely mid-load must not be reported the same way as one whose
// engine died.

// ServingTerms are the facts that decide serving readiness. Every field
// is a fact about the node, never a judgement — the judgement is
// ServingReady below, so that adding a term changes every surface at
// once instead of one of them.
type ServingTerms struct {
	// EngineReady is the pre-existing latch: not disabled, not parked,
	// the serving adapter's health reads ready, and the active model's
	// weights are on disk. It is necessary and — this is the whole point
	// of this type — not sufficient.
	EngineReady bool

	// Paused is the operator's `waired pause`.
	Paused bool

	// Measuring is waired-agent#1127's "this host does not yet know what
	// it costs to use". Already a term of peer admission before this
	// type existed; folded in so there is one list rather than two.
	Measuring bool

	// ModelResident is whether the weights are in (V)RAM, nil when that
	// has not been observed. nil is NOT "cold": docs/decisions/20260820/
	// 0130-model-residency-is-a-setting.md settled that spelling, and
	// docs/decisions/20260822/0218 settled that an unobserved peer is
	// neither promoted nor demoted. Both are honoured below.
	ModelResident *bool

	// ModelLoading is "a load into memory is in flight right now", and it
	// is the term that gates. A request arriving now does not start the
	// load; it QUEUES BEHIND one already running, which is the 148 s the
	// issue measured.
	//
	// ModelResident does not gate, and that is deliberate — see
	// ServingReady. It is carried because it decides the case
	// ModelLoading cannot: for the first seconds after a daemon or engine
	// start nothing has been observed at all.
	ModelLoading bool
}

// Not-ready reasons. These are wire-stable: the probe client puts them
// in the X-Waired-Fallback-Reason header, so they are renamed the way
// any other wire string is renamed.
const (
	NotReadyEngine    = "engine_not_ready"
	NotReadyPaused    = "paused"
	NotReadyMeasuring = "measuring"
	NotReadyModelLoad = "model_loading"
)

// ServingReady reports whether this node may be described as ready.
//
// The residency term gates only through ModelLoading, and the boundary
// is worth stating because it is the difference between two costs:
//
//   - A load is in flight: a request arriving now does not start
//     anything, it waits in line behind a load it cannot shorten. That
//     is the measured defect — 14 s into a 33 s load on the host that
//     filed waired-agent#1307, with every surface reporting ready.
//   - Nothing is in flight and the weights are cold: the request itself
//     is what loads them. Slower than a warm host, and that is exactly
//     what the ranking already expresses —
//     docs/decisions/20260822/0218-residency-breaks-a-tie-not-a-ranking.md
//     rules that a cold peer keeps its place ("cold のままでも勝ち続ける")
//     and only loses a TIE to a warm one. Excluding it here would
//     overturn that ruling, and would starve a mesh whose peers are all
//     cold: nothing would ever be admitted, so nothing would ever warm.
//
// A resident model is ready even with a load in flight: a switch loads
// the new model while the old one keeps answering
// (docs/decisions/20260813/2123).
//
// nil residency is neither promoted nor demoted, per
// docs/decisions/20260820/0130-model-residency-is-a-setting.md — so for
// the seconds after an engine start, where nothing has been observed at
// all, ModelLoading is the only thing that can speak.
func (t ServingTerms) ServingReady() bool {
	return t.NotReadyReason() == ""
}

// NotReadyReason names the first failing term, or "" when ready. The
// order is the one the fallback-reason tag has always used, so the
// string a requester reads for a given peer does not move.
func (t ServingTerms) NotReadyReason() string {
	switch {
	case !t.EngineReady:
		return NotReadyEngine
	case t.Paused:
		return NotReadyPaused
	case t.Measuring:
		return NotReadyMeasuring
	}
	// Resident wins over a load in flight: that load is a replacement,
	// and the model answering now keeps answering.
	if t.ModelResident != nil && *t.ModelResident {
		return ""
	}
	if t.ModelLoading {
		return NotReadyModelLoad
	}
	return ""
}

// ServingTerms extracts the terms from a health snapshot, so a peer's
// answer and this node's own answer are judged by the same function.
func (s HealthSnapshot) ServingTerms() ServingTerms {
	return ServingTerms{
		EngineReady:   s.EngineReady,
		Paused:        s.Paused,
		Measuring:     s.Measuring,
		ModelResident: s.ModelResident,
		ModelLoading:  s.ModelLoading,
	}
}

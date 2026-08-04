package catalog

import (
	"fmt"

	protocatalog "github.com/waired-ai/waired-agent/proto/catalog"
)

// Re-exported retirement table — see proto/catalog/retired.go.
//
// proto holds the FACT (this name is gone, that one replaces it); the
// substitution POLICY is here, because the two consumers want opposite
// things from the same table. The agent migrates a name it is handed; the
// control plane refuses to accept a new one. Same split as InternalOnly
// (the field) versus BundledManifests (what to do about it).
type Retirement = protocatalog.Retirement

// Retirements returns the retirement table.
func Retirements() []Retirement {
	return protocatalog.Retirements()
}

// LookupRetirement finds the retirement that claimed name.
func LookupRetirement(name string) (Retirement, bool) {
	return protocatalog.LookupRetirement(name)
}

// ResolveModel is LookupByAlias plus the retirement table: the resolver
// for a model name that arrives from OUTSIDE this process — a request's
// `model` field, a config file, the control plane's desired_model_id, an
// operator's flag.
//
// The three return values distinguish four states, and callers genuinely
// need different ones:
//
//	(m, zero, true)     name is live                  -> proceed
//	(succ, r, true)     name was retired, substituted -> proceed AND SAY SO
//	(zero, r, false)    retired, successor not in `manifests`
//	                                                  -> "retired", not "unknown"
//	(zero, zero, false) never heard of it             -> today's not-found
//
// The third state cannot happen against the shipped catalog — a proto
// test asserts every successor resolves in it — but a caller passing the
// OFFERED subset, or a test fixture, reaches it, and collapsing it into
// "unknown" would report a model we shipped as one we never had.
//
// SUBSTITUTE WHERE THE NAME IS AN INSTRUCTION — what to fetch, serve,
// route to, tune for. NEVER WHERE IT IS AN OBSERVATION — what is on disk,
// what tier it is, what context window it declares. Those callers keep
// calling LookupByAlias. DeclaredContextWindow is the case that settles
// it: qwen2.5-coder-0.5b is 32k-native and its successor is 262k, so
// substituting there would make a host that is actually serving the old
// 32k weights advertise a 262k window on the mesh. Both kinds occur
// inside one function in resolveTuningTarget (preferred/bundled are
// instructions, state.Active.ModelID is an observation), which is why
// this is a separate resolver rather than a change to LookupByAlias.
func ResolveModel(name string, manifests []Manifest) (Manifest, Retirement, bool) {
	if m, ok := LookupByAlias(name, manifests); ok {
		return m, Retirement{}, true
	}
	r, retired := LookupRetirement(name)
	if !retired {
		return Manifest{}, Retirement{}, false
	}
	m, ok := LookupByAlias(r.SuccessorModelID, manifests)
	if !ok {
		return Manifest{}, r, false
	}
	return m, r, true
}

// RetirementNotice is the one sentence a substituting site says, so every
// surface that reports a substitution reports it the same way and the
// docs can quote one string.
func RetirementNotice(requested string, r Retirement) string {
	return fmt.Sprintf("%q was retired; using %q instead", requested, r.SuccessorModelID)
}

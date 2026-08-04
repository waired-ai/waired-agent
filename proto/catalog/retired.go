package catalog

// Retirement records one model this catalog used to ship and no longer
// does, together with the entry to use in its place.
//
// It is compiled-in data, not a serialised message: nothing here crosses
// the wire, which is why the fields carry no json tags. It lives in the
// proto module because BOTH sides need it and they need OPPOSITE things
// from it. The agent substitutes — a name from a config file, a control
// plane row or an incoming request resolves to the successor and the
// request is served. The control plane rejects — a NEW desired_model_id
// naming a retired entry is an operator asking for something we withdrew,
// and the answer should say so. Neither policy belongs in this package;
// the fact does.
//
// A retired name is reserved FOREVER, the same rule as
// signer.IntegrationOpenCode. Stronger here, in fact: these names are
// also what an engine is asked to load, so re-using one for a different
// model would serve somebody the wrong weights with no version to
// disambiguate it by.
//
// The counterpart to internal/catalog.RetireFailureRate: that constant
// PROPOSES retirements from measured failure rates, this table RECORDS
// the ones actually carried out.
type Retirement struct {
	// Names is every name the deleted entry answered to — its model_id
	// and all of its model_aliases.
	//
	// Listing all of them is load-bearing, not defensive: model_aliases
	// lived inside the JSON file that was deleted, so once the manifest
	// is gone nothing else in the tree can reconstruct them. A retirement
	// that lists only the model_id silently drops every user who pinned
	// the short form.
	//
	// Catalog names only. Source.Tag and Source.RepoID are deliberately
	// NOT listed: a mesh peer still advertising the retired engine tag
	// ceasing to be a routable candidate is what deleting the entry
	// MEANS, and naming the tag here would resurrect the model under a
	// second spelling.
	Names []string

	// SuccessorModelID is the entry to use instead.
	//
	// It must resolve in the shipped catalog, must not be InternalOnly
	// (migrating somebody onto a model we refuse to offer is not a
	// migration), and must not itself be retired — a chain is a migration
	// nobody wrote, and the fix is to re-point the first entry at the
	// final destination. All three are asserted against the real table in
	// retired_test.go.
	SuccessorModelID string

	// Reason is why, with the citation. Same rule as Manifest.InternalOnly:
	// an exemption nobody has to justify is an exemption nobody revisits.
	Reason string
}

// retirements is the table. Entries are never removed and their Names are
// never re-used.
//
// waired/* size aliases do NOT belong here. When an entry that owns one is
// retired, the alias moves into the successor's model_aliases — an alias
// belongs to a live manifest, and an alias parked in this table would
// resolve to a successor while `catalog-tool docs` reported it unassigned.
var retirements = []Retirement{
	{
		Names: []string{
			"qwen2.5-coder-0.5b-instruct",
			"qwen2.5-coder-0.5b",
			"Qwen/Qwen2.5-Coder-0.5B-Instruct",
		},
		SuccessorModelID: "qwen3.5-0.8b",
		Reason: "it never called a tool: 24 of 24 failures on both tool-requiring " +
			"agent-grade cases, a per-call failure rate of at least 90% at 95% confidence " +
			"(waired-ai/waired-agent#475, re-measured #479). Withheld from the offered " +
			"catalog at #475, deleted at #200. qwen3.5-0.8b succeeds it on quality tier " +
			"(12 > 10) and on the harness (3 of 72, bound 0.028).",
	},
}

// Retirements returns the retirement table.
//
// The returned slice is a copy: the table is package data that outlives
// every caller, and handing out the backing array would let one caller's
// sort or append change what the next one reads.
func Retirements() []Retirement {
	out := make([]Retirement, len(retirements))
	copy(out, retirements)
	return out
}

// LookupRetirement finds the retirement that claimed name, whether name
// was the deleted entry's model id or one of its aliases. Empty `name`
// always misses.
//
// A hit does NOT by itself mean "substitute". Read it together with
// LookupByAlias over the shipped manifests, which is where the two
// answers a caller can act on come from:
//
//	LookupByAlias hit                     -> a live entry, use it
//	LookupByAlias miss + this one hits    -> we withdrew it; migrate or say so
//	both miss                             -> never heard of it
//
// Splitting that into an IsValid/IsRetired pair the way proto/signer does
// for integration targets was considered and rejected. That split works
// there because an integration target is a payload-free enum, so the
// valid half can only answer yes. Here the valid half is not a list at
// all — it is the embedded catalog — and a second, hand-maintained set of
// live model ids beside it would drift the first time somebody added a
// manifest.
func LookupRetirement(name string) (Retirement, bool) {
	if name == "" {
		return Retirement{}, false
	}
	for _, r := range retirements {
		for _, n := range r.Names {
			if n == name {
				return r, true
			}
		}
	}
	return Retirement{}, false
}

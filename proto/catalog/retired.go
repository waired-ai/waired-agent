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
// waired/* aliases do NOT belong here, and since #521 none can: the static
// waired/* namespace was retired, so an offered manifest may not declare
// one at all (internal/catalog.TestBundledManifests_Loads). The rule this
// paragraph used to state — that such an alias moves into the successor's
// model_aliases — has nothing left to apply to.
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

	// The 2025 generation (#522). These seven left together and for one
	// reason, which is not a measurement: they are not the generation we
	// carry. #518 pinned the catalog to qwen3.5/3.6, and Hugging Face
	// creation dates split the catalog cleanly at 2025-09-09 / 2026-02-24
	// with nothing in between. Qwen3-Coder is 7 months older than Qwen3.5
	// and 9 months older than Qwen3.6 — the generation before the pinned
	// one, exactly where qwen2.5-coder already sat.
	//
	// The measured half agrees where a measurement exists: qwen2.5-coder-3b
	// failed 12 of 72 agent-grade trials against 4 of 72 for its successor.
	// Above that the entries are `unmeasurable` — no runner we have can
	// hold them — and their successors are unmeasurable too, so the swap
	// loses no evidence.
	{
		Names: []string{
			"qwen2.5-coder-3b-instruct",
			"qwen2.5-coder-3b",
			"Qwen/Qwen2.5-Coder-3B-Instruct",
		},
		SuccessorModelID: "qwen3.5-2b",
		Reason: "the 2025 generation left when #518 pinned the catalog to qwen3.5/3.6 " +
			"(waired-ai/waired-agent#522). Measured worse than its successor where both " +
			"were measurable: 12 of 72 agent-grade trials against qwen3.5-2b's 4 of 72, " +
			"worst case 9 of 24 with a Wilson 95% confidence lower bound of 0.233. It " +
			"also anchored InstallQualityFloorTier, which #522 abolished rather than " +
			"re-anchored.",
	},
	{
		Names: []string{
			"qwen2.5-coder-7b-instruct",
			"qwen2.5-coder-7b",
			"Qwen/Qwen2.5-Coder-7B-Instruct",
		},
		SuccessorModelID: "qwen3.5-4b",
		Reason: "the 2025 generation left when #518 pinned the catalog to qwen3.5/3.6 " +
			"(waired-ai/waired-agent#522). Not a lost comparison: its 32,768-token native " +
			"window put it below the #624 coding-agent floor, so no host had auto-selected " +
			"it since waired-ai/waired#1031. qwen3.5-4b succeeds it at 262,144 native and " +
			"3 of 72 on the harness.",
	},
	{
		Names: []string{
			"qwen2.5-coder-14b-instruct",
			"qwen2.5-coder-14b",
			"Qwen/Qwen2.5-Coder-14B-Instruct",
		},
		SuccessorModelID: "qwen3.5-9b",
		Reason: "the 2025 generation left when #518 pinned the catalog to qwen3.5/3.6 " +
			"(waired-ai/waired-agent#522). Its 32,768-token native window put it below the " +
			"#624 floor on ollama; it survived only on the vllm path, where that floor was " +
			"never applied. qwen3.5-9b succeeds it at 262,144 native and 2 of 72 on the " +
			"harness.",
	},
	{
		Names: []string{
			"qwen3-coder-30b-a3b-instruct",
			"qwen3-coder-30b-a3b",
			"Qwen/Qwen3-Coder-30B-A3B-Instruct",
		},
		SuccessorModelID: "qwen3.6-27b",
		Reason: "the 2025 generation left when #518 pinned the catalog to qwen3.5/3.6 " +
			"(waired-ai/waired-agent#522). Released 2025-07-31, two generations behind. " +
			"qwen3.6-27b succeeds it on quality tier (72 > 68) and on the harness (1 of 72 " +
			"against 0 of 72 — both pass).",
	},
	{
		Names: []string{
			"qwen3-coder-next-80b-a3b-instruct",
			"qwen3-coder-next-80b-a3b",
			"Qwen/Qwen3-Next-80B-A3B-Instruct",
		},
		SuccessorModelID: "qwen3.6-35b-a3b",
		Reason: "the 2025 generation left when #518 pinned the catalog to qwen3.5/3.6 " +
			"(waired-ai/waired-agent#522). Released 2025-09-09, the newest of the seven and " +
			"still the generation before. Unmeasurable on every runner we have (48.5 GB of " +
			"weights, 56 GB minimum RAM), so no measurement is lost; qwen3.6-35b-a3b " +
			"succeeds it and IS measured, at 0 of 72.",
	},
	{
		Names: []string{
			"qwen3-coder-480b-a35b-instruct",
			"qwen3-coder-480b-a35b",
			"Qwen/Qwen3-Coder-480B-A35B-Instruct",
		},
		SuccessorModelID: "glm-5.2",
		Reason: "the 2025 generation left when #518 pinned the catalog to qwen3.5/3.6 " +
			"(waired-ai/waired-agent#522). Released 2025-07-22. Unmeasurable (290 GB of " +
			"weights, no runner available) and so is its successor, so the swap loses no " +
			"evidence. glm-5.2 succeeds it on quality tier (97 > 95) and carries the same " +
			"MIT licence.",
	},
	{
		Names: []string{
			"glm-4.5-air-106b-a12b",
			"glm-4.5-air",
			"zai-org/GLM-4.5-Air",
		},
		SuccessorModelID: "glm-5.2",
		Reason: "the 2025 generation left when #518 pinned the catalog to qwen3.5/3.6 " +
			"(waired-ai/waired-agent#522). The oldest entry in the catalog, released " +
			"2025-07-20. Its 131,072-token native window meant DeclarableNativeWindow " +
			"returned 0 for it, so no host ever auto-selected it. Carried for vendor " +
			"diversity rather than licence, and the licence is covered regardless: glm-5.2 " +
			"and deepseek-v4-flash are both MIT.",
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

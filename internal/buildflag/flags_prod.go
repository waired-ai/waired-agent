//go:build prod

package buildflag

// Production build (`go build -tags prod`): development affordances are
// compiled out so they cannot be re-enabled at runtime. Promoted to staging
// and prd; the same bit-identical artifact is shipped to production after
// passing staging smoke (docs/specs/environments-and-release.md §3.3).
const (
	IsProd             = true
	DefaultEnv         = "prod"
	AllowBypassFlags   = false
	AllowTestEndpoints = false
)

//go:build !prod

package buildflag

// Default (dev) build: every development affordance is available. This is the
// build produced by a plain `go build` and used for local dev, per-PR
// previews, the NAT-traversal testnet, and the dev.waired.net / staging
// pre-cutover environments.
const (
	// IsProd reports whether this is the hardened production build.
	IsProd = false
	// DefaultEnv is the WAIRED_ENV fallback baked into the binary, used to
	// label logs / metrics when the environment variable is unset.
	DefaultEnv = "dev"
	// AllowBypassFlags gates registration of the IdP / auth bypass CLI flags
	// (--bypass-idp, --cookies-insecure, --enable-oidc-grant,
	// --oidc-grant-allowlist, --bypass-cp-iam, --bypass-mode, --bypass-email).
	AllowBypassFlags = true
	// AllowTestEndpoints gates registration of the mock-IdP /test/* routes and
	// the /v1/test/scenario/* and /v1/login/oidc-grant endpoints.
	AllowTestEndpoints = true
)

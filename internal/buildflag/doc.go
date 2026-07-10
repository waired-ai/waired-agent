// Package buildflag is the single source of truth for compile-time feature
// gating between the default (dev) build and the hardened production build
// (`-tags prod`).
//
// The production build physically compiles out development-only affordances —
// the IdP bypass / in-process mock login, insecure-cookie mode, the relay
// CP-IAM bypass, the headless OIDC direct-grant, and the /test/* HTTP routes —
// so they cannot be re-enabled at runtime through a flag or environment
// variable. Call sites gate registration on the AllowBypassFlags /
// AllowTestEndpoints constants; Go's dead-code elimination then drops the
// disabled branches (and the handlers they reference) from the production
// binary, shrinking the attack surface to what a real deployment needs.
//
// The constants live in two build-tagged files (flags_dev.go / flags_prod.go).
// buildflag_test.go locks their invariants so the two files cannot desync.
//
// See docs/specs/environments-and-release.md §3.1 and §6.4.
package buildflag

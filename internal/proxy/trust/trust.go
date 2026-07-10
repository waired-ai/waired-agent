// Package trust REMOVES the artifacts of the retired transparent-proxy MITM
// root CA: the CA anchored in the OS trust store and the NODE_EXTRA_CA_CERTS
// bridge that Claude Code (a Node app, which ignores the OS trust store) used
// to trust the minted leaves.
//
// The install side is gone with the MITM proxy (#488); only the uninstall path
// survives so an upgraded host can be cleaned up. The OS-specific work lives in
// the build-tagged files; this file holds the OS-agnostic constants. Operations
// require elevation and are invoked from internal/proxy/legacycleanup.
package trust

// CAStoreFileName is the filename used under the OS CA-store directory by the
// retired MITM proxy; the uninstall path removes exactly this anchor.
const CAStoreFileName = "waired-claude-proxy.crt"

// CACommonName is the X.509 CommonName the retired MITM CA was minted with. The
// OS untrust path matches the store entry by this name (certutil -delstore on
// Windows, `security delete-certificate -c` on macOS).
const CACommonName = "waired Claude proxy CA"

#!/usr/bin/env bash
# install-script-lint.sh — shellcheck over the shell that reaches a user
# machine (the curl|sh installer, the .deb maintainer scripts) plus every
# CI guard/helper script in this directory. Run it with
# `make install-script-lint`; ci.yml's install-scripts job does the same.
# The PowerShell half is scripts/ci/ps-script-lint.ps1.
#
# Why the target list lives HERE and not in the Makefile (#292): the
# Makefile is listed whole in scripts/ci/testnet-relevant-paths.txt —
# correctly, since dist-agent / build-agent / dist-agent-testharness
# produce the binaries and harness inputs the real-NAT testnet consumes —
# and testnet-gate.sh matches by plain path prefix, deliberately, so the
# policy file stays trivially auditable. That means the gate cannot see
# WHICH target a Makefile diff touched: adding one script name to this
# list used to arm a ~25-minute testnet run on PRs that change nothing
# shipped. Adding lint targets here costs nothing. Keep it that way — do
# not move this list back into the Makefile.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

command -v shellcheck >/dev/null 2>&1 || {
  echo "error: shellcheck not found in PATH" >&2
  exit 1
}

# Shipped to user machines: the install one-liner and its uninstaller,
# plus the desktop-integration helper the .deb runs.
shellcheck packaging/install/install.sh \
           packaging/install/uninstall.sh \
           build/install-desktop.sh

# .deb maintainer scripts — these run as root on every install/remove.
# Listed explicitly rather than globbed: the set is the packaging
# contract (waired-tray has no prerm), not whatever happens to be there.
shellcheck packaging/debian/waired/postinst \
           packaging/debian/waired/prerm \
           packaging/debian/waired/postrm \
           packaging/debian/waired-tray/postinst \
           packaging/debian/waired-tray/postrm

# Every CI script, discovered rather than enumerated. The hand-kept list
# this replaced had drifted eight scripts behind — including the testnet
# gate's own — because a new guard is exactly the kind of file whose
# enumeration gets forgotten. Globbing removes the failure mode and the
# edit. The Go guards (lookpathguard/, protoconsumer/, protoguard/) are
# directories, so they do not match; this script lints itself, the same
# way ps-script-lint.ps1 does.
shellcheck scripts/ci/*.sh

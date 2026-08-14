#!/usr/bin/env bash
# resolve-build-version.sh — the two version strings every artifact of a
# build is stamped with, resolved once so all four legs agree.
#
#   semver -> Go buildinfo (ldflags VERSION), Windows Inno AppVersion,
#             macOS VERSION file. SemVer spelling.
#   debver -> the .deb Version field (PKG_VERSION). nfpm's version_schema
#             is 'none', so the SemVer-`-` -> Debian-`~` rewrite is here.
#
# Two inputs, both from the environment so the decision is a pure
# function of them and the test below can drive it:
#
#   REF_TYPE       "tag" for a release build, anything else for edge
#   REF_NAME       the tag name on a tag build (e.g. v0.0.3-rc1)
#   BUILD_TS       edge only: the UTC timestamp (yyyymmddHHMMSS)
#   BUILD_SHA      edge only: the short commit sha
#   LATEST_TAG     edge only: the newest v* tag, for the numeric core
#
# Prints two `name=value` lines on stdout; the workflow appends them to
# $GITHUB_OUTPUT. Why a script and not the inline `run:` block it
# replaces: an inline block cannot be executed by a test, and the rewrite
# below shipped unimplemented on the tag branch for nine releases
# precisely because nothing exercised it (waired-agent#780).
#
# Usage:
#   REF_TYPE=tag REF_NAME=v0.0.3-rc1 bash scripts/ci/resolve-build-version.sh
set -euo pipefail

REF_TYPE="${REF_TYPE:-}"
REF_NAME="${REF_NAME:-}"

if [ "${REF_TYPE}" = "tag" ]; then
  v="${REF_NAME#v}"
  semver="${v}"
  # SemVer's `-` -> Debian's `~`, EVERY occurrence.
  #
  # dpkg splits a version at the LAST hyphen into upstream and revision,
  # and orders an absent revision below a present one, so a hyphen makes
  # a prerelease sort ABOVE the release it leads to and a two-hyphen tag
  # sort above the next single-hyphen one. Both were measured on the
  # published repository (waired-ai/waired#1217):
  #
  #   0.0.2-rc9  >  0.0.2          every -rcN outranked its own release
  #   0.0.2-rc9  <  0.0.2-rc8-dev  a preview tag outranked the next rc
  #
  # `~` sorts below everything including the end of a part, which is
  # exactly the prerelease ordering wanted, and rewriting every hyphen
  # (not just the first) means the result never depends on that
  # last-hyphen split at all.
  debver="${v//-/\~}"
else
  core="${LATEST_TAG:-}"
  core="${core#v}"
  core="${core%%[-+]*}"
  [ -n "${core}" ] || core="0.0.0"
  semver="${core}-edge.${BUILD_TS}+${BUILD_SHA}"
  debver="${core}~edge.${BUILD_TS}+${BUILD_SHA}"
fi

printf 'semver=%s\n' "${semver}"
printf 'debver=%s\n' "${debver}"

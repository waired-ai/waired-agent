#!/usr/bin/env bash
# win-fileversion.sh — turn a Waired semver into the four-integer form a
# Windows VERSIONINFO resource needs.
#
# A VERSIONINFO resource has two versions in it, and they are not the same
# kind of thing:
#
#   FixedFileInfo.FileVersion   four 16-bit integers. No prerelease, no
#                               build metadata, no text. This is what the
#                               Win32 version API compares numerically.
#   StringFileInfo.FileVersion  free text. This is what Explorer's Properties
#                               dialog SHOWS, so it carries the full semver.
#
# This script produces the first from the second. The build passes the
# untouched semver to goversioninfo's -file-version, so nothing is lost:
# `0.0.3-rc1` shows as `0.0.3-rc1` and compares as `0.0.3.0`.
#
# The fourth field is always 0. There is no honest place to put `rc1` in it:
# a prerelease numbered into the last slot would sort ABOVE the release it
# precedes, which is worse than not encoding it at all.
#
# Usage: win-fileversion.sh <semver>
#
#   0.0.3                          -> 0.0.3.0
#   v1.2.3                         -> 1.2.3.0
#   0.0.3-rc1                      -> 0.0.3.0
#   0.0.3-edge.20260905123456+abc  -> 0.0.3.0
#   0.0.0-dev.gabc1234             -> 0.0.0.0
#
# An input this cannot read is an ERROR, not a `0.0.0.0`. Falling back
# silently is how every build ends up reporting the same version, which is
# the defect this exists to fix (waired-ai/waired-agent#1209).
set -euo pipefail

if [ "$#" -ne 1 ]; then
  printf 'usage: %s <semver>\n' "${0##*/}" >&2
  exit 2
fi

semver="$1"
core="${semver#v}"
# Cut prerelease and build metadata: everything from the first - or + on.
core="${core%%-*}"
core="${core%%+*}"

if ! printf '%s' "${core}" | grep -qE '^[0-9]+\.[0-9]+\.[0-9]+$'; then
  printf 'win-fileversion: cannot read a major.minor.patch out of %q\n' "${semver}" >&2
  exit 1
fi

IFS='.' read -r major minor patch <<<"${core}"
for part in "${major}" "${minor}" "${patch}"; do
  # Each field is a 16-bit integer in the resource; a larger one wraps
  # silently rather than failing, so refuse it here.
  if [ "${part}" -gt 65535 ]; then
    printf 'win-fileversion: %s does not fit in a 16-bit version field (%s)\n' "${part}" "${semver}" >&2
    exit 1
  fi
done

printf '%d.%d.%d.0\n' "${major}" "${minor}" "${patch}"

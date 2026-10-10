#!/bin/sh
# Print the action.yml for a release: where Probe downloads each executable
# from, and the SHA-256 digest it must have.
#
#   scripts/action-yml.sh <tag> <checksums.txt>
set -eu

tag=$1
checksums=$2
repo=${GITHUB_REPOSITORY:-mozership/probe-jmap}

cat <<YAML
name: jmap
description: Call JMAP methods
guard: [read-only, allow-host]
params: [url, calls, using, using_also, account_id, basic_auth, headers, timeout]
runs:
  using: binary
  url: https://github.com/${repo}/releases/download/${tag}/probe-jmap_{os}_{arch}
  checksums:
YAML
# A line of checksums.txt is "<digest>  probe-jmap_<os>_<arch>".
awk '$2 ~ /^probe-jmap_/ { sub(/^probe-jmap_/, "", $2); printf "    %s: \"%s\"\n", $2, $1 }' "$checksums" | sort

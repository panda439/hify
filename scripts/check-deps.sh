#!/usr/bin/env bash
# Enforces the 5-layer module dependency direction from CLAUDE.md: a package
# may only import packages assigned to a strictly lower layer. Run via
# `make check-deps` after adding/changing imports in any internal/ module.
# Written for bash 3.2 (macOS's default /bin/bash) — no associative arrays.
set -euo pipefail
cd "$(dirname "$0")/.."


# Only the 8 real business modules are subject to the layer/same-layer
# rule. platform/config/db are pure infrastructure, not "modules" with a
# Service interface — every layer, including layer 0's own `user`, is
# expected to import them freely, so they're intentionally left unmapped
# (layer_of returns empty for them, which the caller treats as "skip").
layer_of() {
  case "$1" in
    user) echo 0 ;;
    auth|provider|mcp) echo 1 ;;
    agent|knowledge) echo 2 ;;
    conversation) echo 3 ;;
    workflow) echo 4 ;;
    *) echo "" ;;
  esac
}

fail=0

while IFS=$'\t' read -r import_path deps_json; do
  rel=${import_path#hify/internal/}
  pkg=${rel%%/*}
  layer=$(layer_of "$pkg")
  [ -z "$layer" ] && continue

  while IFS= read -r dep; do
    [ -z "$dep" ] && continue
    case "$dep" in
      hify/internal/*) ;;
      *) continue ;;
    esac
    drel=${dep#hify/internal/}
    dpkg=${drel%%/*}
    [ "$dpkg" = "$pkg" ] && continue
    dlayer=$(layer_of "$dpkg")
    [ -z "$dlayer" ] && continue
    if [ "$dlayer" -ge "$layer" ]; then
      echo "DEPENDENCY VIOLATION: $import_path (layer $layer) imports $dep (layer $dlayer)"
      fail=1
    fi
  done < <(echo "$deps_json" | jq -r '.[]')
done < <(go list -json ./internal/... | jq -r '[.ImportPath, (.Imports // [] | tojson)] | @tsv')

if [ "$fail" -eq 0 ]; then
  echo "check-deps: OK - no cross-layer or same-layer violations"
else
  exit 1
fi

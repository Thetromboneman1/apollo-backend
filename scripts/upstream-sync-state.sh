#!/usr/bin/env bash
# Read-only classification. Callers decide whether a candidate may be validated.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "Usage: $0 <downstream-commit> <upstream-commit>" >&2
  exit 2
fi

downstream=$(git rev-parse --verify --end-of-options "$1^{commit}")
upstream=$(git rev-parse --verify --end-of-options "$2^{commit}")

is_ancestor() {
  local status
  if git merge-base --is-ancestor "$1" "$2"; then
    return 0
  else
    status=$?
    if [[ "$status" -ne 1 ]]; then
      echo "Unable to classify synchronization history (git exit $status)." >&2
      exit "$status"
    fi
    return 1
  fi
}

if [[ "$downstream" == "$upstream" ]]; then
  echo equal
elif is_ancestor "$upstream" "$downstream"; then
  echo upstream-contained
elif is_ancestor "$downstream" "$upstream"; then
  echo fast-forward
else
  echo diverged
fi

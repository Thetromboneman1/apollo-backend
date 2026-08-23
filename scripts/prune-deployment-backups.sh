#!/usr/bin/env bash
set -euo pipefail

backup_root="${DEPLOYMENT_BACKUP_DIR:-/home/apollo/backups/apollo-backend}"
retention_days="${DEPLOYMENT_BACKUP_RETENTION_DAYS:-30}"

[[ "$retention_days" =~ ^[0-9]+$ ]] && (( retention_days >= 1 && retention_days <= 3650 )) || {
  echo "DEPLOYMENT_BACKUP_RETENTION_DAYS must be between 1 and 3650" >&2
  exit 1
}
[[ -d "$backup_root" ]] || exit 0
backup_root="$(readlink -f "$backup_root")"
case "$backup_root" in
  /|/home|/home/apollo)
    echo "refusing unsafe backup root: $backup_root" >&2
    exit 1
    ;;
esac

while IFS= read -r -d '' candidate; do
  [[ "$(dirname "$candidate")" == "$backup_root" ]] || {
    echo "refusing candidate outside backup root" >&2
    exit 1
  }
  name="$(basename "$candidate")"
  [[ "$name" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] || continue
  rm -rf -- "$candidate"
done < <(find "$backup_root" -mindepth 1 -maxdepth 1 -type d   -mtime "+$retention_days" -print0)

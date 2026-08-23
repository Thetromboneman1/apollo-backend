#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"

env_file="$repo_dir/.env.docker"
with_bark=0
max_attempts="${MAX_DEPLOY_ATTEMPTS:-2}"

usage() {
  echo "usage: $0 [--env-file PATH] [--with-bark] [--public-check] [--attempts 1..3]" >&2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env-file)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      env_file="$2"
      shift 2
      ;;
    --with-bark) with_bark=1; shift ;;
    --public-check) shift ;;
    --attempts)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      max_attempts="$2"
      shift 2
      ;;
    -h|--help) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done

case "$max_attempts" in
  1|2|3) ;;
  *) echo "attempts must be between 1 and 3" >&2; exit 2 ;;
esac

for command in curl docker git python3 systemctl; do
  command -v "$command" >/dev/null || { echo "missing required command: $command" >&2; exit 1; }
done

validate_args=(--env-file "$env_file" --config-only)
if [[ $with_bark -eq 1 ]]; then validate_args+=(--with-bark); fi
"$repo_dir/scripts/validate-deployment.sh" "${validate_args[@]}"

cloudflared_unit="${CLOUDFLARED_SYSTEMD_UNIT:-cloudflared-apollo.service}"
[[ "$cloudflared_unit" =~ ^[A-Za-z0-9@_.-]+\.service$ ]] || {
  echo "unsafe CLOUDFLARED_SYSTEMD_UNIT value" >&2
  exit 1
}
systemctl is-enabled --quiet "$cloudflared_unit" || {
  echo "$cloudflared_unit must be enabled before deployment" >&2
  exit 1
}
systemctl is-active --quiet "$cloudflared_unit" || {
  echo "$cloudflared_unit must be active before deployment" >&2
  exit 1
}
if [[ -n "$(git status --porcelain --untracked-files=all)" ]]; then
  echo "deployment requires a clean committed checkout" >&2
  exit 1
fi

compose=(docker compose --env-file "$env_file")
if [[ $with_bark -eq 1 ]]; then compose+=(--profile bark); fi

backup_dir=""
if APOLLO_ENV_FILE="$env_file" "${compose[@]}" ps --status running --services 2>/dev/null | grep -qx postgres; then
  backup_args=(--env-file "$env_file")
  if [[ $with_bark -eq 1 ]]; then backup_args+=(--with-bark); fi
  backup_dir="$("$repo_dir/scripts/backup-deployment.sh" "${backup_args[@]}")"
  echo "pre-deployment backup: $backup_dir"
fi

APOLLO_ENV_FILE="$env_file" "${compose[@]}" pull --quiet

attempt=1
while [[ $attempt -le $max_attempts ]]; do
  echo "deployment attempt $attempt of $max_attempts"
  if APOLLO_ENV_FILE="$env_file" "${compose[@]}" up -d --no-build --remove-orphans     --wait --wait-timeout 180; then
    post_args=(--env-file "$env_file")
    if [[ $with_bark -eq 1 ]]; then post_args+=(--with-bark); fi
    post_args+=(--public)
    if "$repo_dir/scripts/validate-deployment.sh" "${post_args[@]}"; then
      echo "deployment complete: https://apollo.connerclan.com"
      exit 0
    fi
  fi
  attempt=$((attempt + 1))
done

if [[ -n "$backup_dir" ]]; then
  echo "deployment validation failed; restoring prior container images" >&2
  rollback_args=("$backup_dir" --confirm --env-file "$env_file")
  if [[ $with_bark -eq 1 ]]; then rollback_args+=(--with-bark); fi
  "$repo_dir/scripts/rollback-deployment.sh" "${rollback_args[@]}"
else
  echo "deployment failed and no prior running database was available for rollback" >&2
fi
exit 1

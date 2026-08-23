#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"

env_file="$repo_dir/.env.docker"
with_bark=0
restore_data=0
confirmed=0
backup_dir=""

usage() {
  echo "usage: $0 BACKUP_DIR --confirm [--env-file PATH] [--with-bark] [--restore-data]" >&2
}

[[ $# -gt 0 ]] || { usage; exit 2; }
backup_dir="$1"
shift
while [[ $# -gt 0 ]]; do
  case "$1" in
    --confirm) confirmed=1; shift ;;
    --env-file)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      env_file="$2"
      shift 2
      ;;
    --with-bark) with_bark=1; shift ;;
    --restore-data) restore_data=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done

[[ $confirmed -eq 1 ]] || { echo "rollback requires --confirm" >&2; exit 1; }
[[ -f "$backup_dir/rollback-images.yml" ]] || { echo "invalid backup: missing rollback-images.yml" >&2; exit 1; }
[[ -f "$backup_dir/rollback-services.txt" ]] || { echo "invalid backup: missing rollback-services.txt" >&2; exit 1; }
[[ -f "$backup_dir/rollback-running-services.txt" ]] || { echo "invalid backup: missing rollback-running-services.txt" >&2; exit 1; }
[[ -f "$backup_dir/docker-compose.yml" ]] || { echo "invalid backup: missing docker-compose.yml" >&2; exit 1; }
[[ -f "$backup_dir/project-name.txt" ]] || { echo "invalid backup: missing project-name.txt" >&2; exit 1; }
[[ -f "$backup_dir/images.tar" ]] || { echo "invalid backup: missing images.tar" >&2; exit 1; }
[[ -f "$backup_dir/SHA256SUMS" ]] || { echo "invalid backup: missing SHA256SUMS" >&2; exit 1; }
[[ -f "$env_file" ]] || { echo "missing environment file: $env_file" >&2; exit 1; }
for command in readlink sha256sum; do
  command -v "$command" >/dev/null || { echo "missing required command: $command" >&2; exit 1; }
done
(
  cd "$backup_dir"
  sha256sum --check --quiet SHA256SUMS
) || { echo "backup integrity verification failed" >&2; exit 1; }

[[ -L "$backup_dir/secrets" ]] || {
  echo "backup is missing its live-secrets link" >&2
  exit 1
}
live_secrets="$(readlink -f "$backup_dir/secrets")"
[[ "$live_secrets" == "$(readlink -f "$repo_dir/secrets")" && -d "$live_secrets" ]] || {
  echo "backup live-secrets link does not resolve to this checkout" >&2
  exit 1
}
current_environment_hash="$(sha256sum "$env_file" | awk '{print $1}')"
captured_environment_hash="$(sed -n '1p' "$backup_dir/environment.sha256")"
if [[ "$current_environment_hash" != "$captured_environment_hash" ]]; then
  echo "warning: rollback uses the current environment and secrets; their captured hash differs" >&2
fi

project_name="$(sed -n '1p' "$backup_dir/project-name.txt")"
[[ "$project_name" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || {
  echo "invalid backup: unsafe Compose project name" >&2
  exit 1
}

compose=(docker compose --project-name "$project_name" --env-file "$env_file")
if [[ $with_bark -eq 1 ]]; then compose+=(--profile bark); fi

rollback_services=()
while IFS= read -r service; do
  [[ "$service" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || {
    echo "invalid backup: unsafe service name" >&2
    exit 1
  }
  rollback_services+=("$service")
done < "$backup_dir/rollback-services.txt"
[[ ${#rollback_services[@]} -gt 0 ]] || { echo "invalid backup: empty service list" >&2; exit 1; }

rollback_running_services=()
while IFS= read -r service; do
  [[ "$service" =~ ^[A-Za-z0-9][A-Za-z0-9_.-]*$ ]] || {
    echo "invalid backup: unsafe running service name" >&2
    exit 1
  }
  grep -qx "$service" "$backup_dir/rollback-services.txt" || {
    echo "invalid backup: running service is absent from captured services" >&2
    exit 1
  }
  rollback_running_services+=("$service")
done < "$backup_dir/rollback-running-services.txt"
[[ ${#rollback_running_services[@]} -gt 0 ]] || { echo "invalid backup: empty running service list" >&2; exit 1; }

if grep -qx bark-server "$backup_dir/rollback-services.txt" && [[ $with_bark -ne 1 ]]; then
  echo "backup includes bark-server; rerun with --with-bark" >&2
  exit 1
fi

docker image load --input "$backup_dir/images.tar" >/dev/null

if [[ $restore_data -eq 1 ]]; then
  [[ -f "$backup_dir/postgres.dump" ]] || { echo "invalid backup: missing postgres.dump" >&2; exit 1; }

  APOLLO_ENV_FILE="$env_file" "${compose[@]}" exec -T postgres \
    pg_restore --list < "$backup_dir/postgres.dump" >/dev/null

  safety_args=(--env-file "$env_file")
  if [[ $with_bark -eq 1 ]]; then safety_args+=(--with-bark); fi
  safety_backup="$(COMPOSE_PROJECT_NAME="$project_name" \
    "$repo_dir/scripts/backup-deployment.sh" "${safety_args[@]}")"
  echo "current-state safety backup: $safety_backup" >&2

  restore_suffix="$(date -u +%Y%m%d%H%M%S)"
  restore_db="apollo_restore_$restore_suffix"
  previous_db="apollo_pre_restore_$restore_suffix"
  echo "testing database restore in $restore_db" >&2
  APOLLO_ENV_FILE="$env_file" "${compose[@]}" exec -T postgres \
    createdb -U apollo "$restore_db"
  if ! APOLLO_ENV_FILE="$env_file" "${compose[@]}" exec -T postgres \
    pg_restore -U apollo -d "$restore_db" --exit-on-error < "$backup_dir/postgres.dump"; then
    echo "test restore failed; current database was not changed and $restore_db was retained for inspection" >&2
    exit 1
  fi
  APOLLO_ENV_FILE="$env_file" "${compose[@]}" exec -T postgres \
    psql -U apollo -d "$restore_db" -v ON_ERROR_STOP=1 -Atq -c "SELECT 1;" >/dev/null

  echo "swapping the verified database into service" >&2
  APOLLO_ENV_FILE="$env_file" "${compose[@]}" stop \
    origin-gateway api scheduler worker-notifications worker-stuck-notifications \
    worker-subreddits worker-trending worker-users worker-live-activities pgbouncer
  APOLLO_ENV_FILE="$env_file" "${compose[@]}" exec -T postgres \
    psql -U apollo -d postgres -v ON_ERROR_STOP=1 -c \
    "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname IN ('apollo', '$restore_db') AND pid <> pg_backend_pid();" >/dev/null
  APOLLO_ENV_FILE="$env_file" "${compose[@]}" exec -T postgres \
    psql -U apollo -d postgres -v ON_ERROR_STOP=1 -c \
    "ALTER DATABASE apollo RENAME TO $previous_db; ALTER DATABASE $restore_db RENAME TO apollo;" >/dev/null

  if [[ $with_bark -eq 1 && -d "$backup_dir/barkdata" ]]; then
    APOLLO_ENV_FILE="$env_file" "${compose[@]}" stop bark-server
    bark_id="$(APOLLO_ENV_FILE="$env_file" "${compose[@]}" ps -a -q bark-server | head -n 1)"
    [[ -n "$bark_id" ]] && docker cp "$backup_dir/barkdata/." "$bark_id:/data/"
  fi
  echo "database restore verified before activation; prior database retained as $previous_db" >&2
  echo "current-state safety backup remains at $safety_backup" >&2
fi

rollback_compose=(docker compose --project-directory "$backup_dir" --project-name "$project_name" \
  --env-file "$env_file" -f "$backup_dir/docker-compose.yml" -f "$backup_dir/rollback-images.yml")
if [[ $with_bark -eq 1 ]]; then rollback_compose+=(--profile bark); fi
APOLLO_ENV_FILE="$env_file" "${rollback_compose[@]}" config --quiet
APOLLO_ENV_FILE="$env_file" "${compose[@]}" stop
APOLLO_ENV_FILE="$env_file" "${rollback_compose[@]}" up -d --no-build --no-deps \
  --force-recreate --remove-orphans --wait --wait-timeout 120 "${rollback_running_services[@]}"

validate_args=(--env-file "$env_file" --public)
if [[ $with_bark -eq 1 ]]; then validate_args+=(--with-bark); fi
COMPOSE_PROJECT_NAME="$project_name" "$repo_dir/scripts/validate-deployment.sh" "${validate_args[@]}"
echo "captured deployment state restored from $backup_dir"

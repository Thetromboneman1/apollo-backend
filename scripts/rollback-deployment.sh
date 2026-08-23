#!/usr/bin/env bash
set -euo pipefail

invocation_dir="$PWD"
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
for command in awk docker python3 readlink sha256sum stat; do
  command -v "$command" >/dev/null || { echo "missing required command: $command" >&2; exit 1; }
done
case "$backup_dir" in
  /*) ;;
  *) backup_dir="$invocation_dir/$backup_dir" ;;
esac
[[ -d "$backup_dir" ]] || { echo "invalid backup directory: $backup_dir" >&2; exit 1; }
backup_dir="$(readlink -f -- "$backup_dir")"
case "$env_file" in
  /*) ;;
  *) env_file="$invocation_dir/$env_file" ;;
esac
[[ -f "$env_file" ]] || { echo "missing environment file: $env_file" >&2; exit 1; }
env_file="$(readlink -f -- "$env_file")"

[[ -f "$backup_dir/rollback-images.yml" ]] || { echo "invalid backup: missing rollback-images.yml" >&2; exit 1; }
[[ -f "$backup_dir/rollback-services.txt" ]] || { echo "invalid backup: missing rollback-services.txt" >&2; exit 1; }
[[ -f "$backup_dir/rollback-running-services.txt" ]] || { echo "invalid backup: missing rollback-running-services.txt" >&2; exit 1; }
[[ -f "$backup_dir/docker-compose.yml" ]] || { echo "invalid backup: missing docker-compose.yml" >&2; exit 1; }
[[ -f "$backup_dir/project-name.txt" ]] || { echo "invalid backup: missing project-name.txt" >&2; exit 1; }
[[ -f "$backup_dir/images.tar" ]] || { echo "invalid backup: missing images.tar" >&2; exit 1; }
[[ -f "$backup_dir/SHA256SUMS" ]] || { echo "invalid backup: missing SHA256SUMS" >&2; exit 1; }
required_backup_files=(
  rollback-images.yml rollback-services.txt rollback-running-services.txt
  docker-compose.yml project-name.txt images.tar
  environment.sha256 git-revision.txt
  apollo-image-reference.txt apollo-image-id.txt
  docs/deployment/nginx.conf docs/schema.sql docker/migrate.sh
  migrations/000013_restore_live_activities.up.sql
  migrations/000014_add_device_transport.up.sql
)
if [[ $restore_data -eq 1 ]]; then
  required_backup_files+=(postgres.dump)
fi
for relative_path in "${required_backup_files[@]}"; do
  [[ -f "$backup_dir/$relative_path" ]] || {
    echo "invalid backup: missing $relative_path" >&2
    exit 1
  }
done

require_other_permission() {
  local relative_path="$1"
  local permission_bit="$2"
  local description="$3"
  local mode mode_number
  mode="$(stat -c '%a' "$backup_dir/$relative_path")"
  mode_number=$((8#$mode))
  if (( (mode_number & permission_bit) == 0 )); then
    echo "invalid backup: $relative_path is not $description by its container user" >&2
    exit 1
  fi
}
require_other_permission docs/deployment/nginx.conf 4 readable
require_other_permission docs/schema.sql 4 readable
require_other_permission migrations/000013_restore_live_activities.up.sql 4 readable
require_other_permission migrations/000014_add_device_transport.up.sql 4 readable
require_other_permission docker/migrate.sh 4 readable
require_other_permission docker/migrate.sh 1 executable

(
  cd "$backup_dir"
  sha256sum --check --strict --quiet SHA256SUMS
) || { echo "backup integrity verification failed" >&2; exit 1; }
for relative_path in "${required_backup_files[@]}"; do
  checksum_path="./$relative_path"
  awk -v wanted="$checksum_path" '
    $2 == wanted { count++ }
    END { exit count == 1 ? 0 : 1 }
  ' "$backup_dir/SHA256SUMS" || {
    echo "invalid backup: $checksum_path must appear exactly once in SHA256SUMS" >&2
    exit 1
  }
done

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

mapfile -t captured_image_references < "$backup_dir/apollo-image-reference.txt"
[[ ${#captured_image_references[@]} -eq 1 ]] || {
  echo "invalid backup: Apollo image reference must contain exactly one line" >&2
  exit 1
}
captured_image_reference="${captured_image_references[0]}"
[[ "$captured_image_reference" =~ ^ghcr\.io/[a-z0-9._-]+/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$ ]] || {
  echo "invalid backup: unsafe Apollo image reference" >&2
  exit 1
}
mapfile -t captured_image_ids < "$backup_dir/apollo-image-id.txt"
[[ ${#captured_image_ids[@]} -eq 1 ]] || {
  echo "invalid backup: Apollo image ID must contain exactly one line" >&2
  exit 1
}
captured_image_id="${captured_image_ids[0]}"
[[ "$captured_image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "invalid backup: unsafe Apollo image ID" >&2
  exit 1
}

project_name="$(sed -n '1p' "$backup_dir/project-name.txt")"
[[ "$project_name" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || {
  echo "invalid backup: unsafe Compose project name" >&2
  exit 1
}

compose=(docker compose --project-directory "$repo_dir" --project-name "$project_name" \
  --env-file "$env_file" -f "$repo_dir/docker-compose.yml")
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

if [[ $with_bark -eq 1 ]]; then
  grep -qx bark-server "$backup_dir/rollback-services.txt" || {
    echo "invalid backup: --with-bark requires a captured bark-server service" >&2
    exit 1
  }
  grep -qx bark-server "$backup_dir/rollback-running-services.txt" || {
    echo "invalid backup: --with-bark requires bark-server to have been captured running" >&2
    exit 1
  }
else
  if grep -qx bark-server "$backup_dir/rollback-services.txt"; then
    echo "backup includes bark-server; rerun with --with-bark" >&2
    exit 1
  fi
fi

app_services=(
  api scheduler worker-notifications worker-stuck-notifications
  worker-subreddits worker-trending worker-users worker-live-activities
)
essential_services=(
  postgres pgbouncer redis-queue redis-locks api origin-gateway scheduler
  worker-notifications worker-stuck-notifications worker-subreddits
  worker-trending worker-users worker-live-activities
)
for service in "${essential_services[@]}"; do
  grep -qx "$service" "$backup_dir/rollback-services.txt" || {
    echo "invalid backup: missing required service $service" >&2
    exit 1
  }
  grep -qx "$service" "$backup_dir/rollback-running-services.txt" || {
    echo "invalid backup: required service $service was not captured running" >&2
    exit 1
  }
done

current_config_args=(--env-file "$env_file" --config-only)
if [[ $with_bark -eq 1 ]]; then current_config_args+=(--with-bark); fi
COMPOSE_PROJECT_NAME="$project_name" \
  "$repo_dir/scripts/validate-deployment.sh" "${current_config_args[@]}"

rollback_compose=(docker compose --project-directory "$backup_dir" --project-name "$project_name" \
  --env-file "$env_file" -f "$backup_dir/docker-compose.yml" -f "$backup_dir/rollback-images.yml")
if [[ $with_bark -eq 1 ]]; then rollback_compose+=(--profile bark); fi
APOLLO_ENV_FILE="$env_file" "${rollback_compose[@]}" config --quiet
APOLLO_ENV_FILE="$env_file" "${rollback_compose[@]}" config --format json | python3 -c '
import json, sys

cfg = json.load(sys.stdin)
expected_id = sys.argv[1]
expected_reference = sys.argv[2]
label = "com.apollo-reborn.deployment.image-reference"
for name in sys.argv[3:]:
    service = cfg.get("services", {}).get(name) or {}
    if service.get("image") != expected_id:
        raise SystemExit(name + " does not use the captured Apollo image ID")
    if service.get("pull_policy") != "never":
        raise SystemExit(name + " does not disable image pulling")
    if service.get("build"):
        raise SystemExit(name + " unexpectedly has a build definition")
    if (service.get("labels") or {}).get(label) != expected_reference:
        raise SystemExit(name + " lacks the captured Apollo image reference label")
' "$captured_image_id" "$captured_image_reference" "${app_services[@]}"

docker image load --input "$backup_dir/images.tar" >/dev/null
loaded_image_id="$(docker image inspect --format '{{.Id}}' "$captured_image_id" 2>/dev/null)" || {
  echo "captured Apollo image ID was not restored from images.tar" >&2
  exit 1
}
[[ "$loaded_image_id" == "$captured_image_id" ]] || {
  echo "captured Apollo image ID does not match the loaded image" >&2
  exit 1
}
if resolved_reference_id="$(docker image inspect --format '{{.Id}}' "$captured_image_reference" 2>/dev/null)"; then
  [[ "$resolved_reference_id" == "$captured_image_id" ]] || {
    echo "captured Apollo image reference resolves to a different local image" >&2
    exit 1
  }
fi

print_recovery_command() {
  printf '  ' >&2
  printf '%q ' "$@" >&2
  printf '\n' >&2
}

report_post_activation_failure() {
  local exit_status="$1"
  local database_names database_query_status database_name recovery_sql
  local -a recovery_stop_services recovery_validate_args

  trap - ERR
  set +e
  echo "rollback failed after the restored database was activated" >&2
  echo "active restored database: apollo" >&2
  echo "retained prior database: $previous_db" >&2
  echo "current-state safety backup: $safety_backup" >&2
  echo "failed restored database retention name: $failed_restore_db" >&2

  database_names="$(APOLLO_ENV_FILE="$env_file" "${compose[@]}" exec -T postgres \
    psql -U apollo -d postgres -Atq -c \
    "SELECT datname FROM pg_database WHERE datname IN ('apollo', '$previous_db') ORDER BY datname;" \
    2>/dev/null)"
  database_query_status=$?
  if [[ $database_query_status -eq 0 ]]; then
    echo "database names currently present:" >&2
    while IFS= read -r database_name; do
      [[ -n "$database_name" ]] && echo "  $database_name" >&2
    done <<< "$database_names"
  else
    echo "database names currently present: unavailable (postgres is not reachable)" >&2
  fi

  echo "current Compose state:" >&2
  APOLLO_ENV_FILE="$env_file" "${compose[@]}" ps -a >&2 || \
    echo "  unavailable (Docker Compose state could not be read)" >&2

  recovery_stop_services=(origin-gateway "${app_services[@]}" pgbouncer)
  if [[ $with_bark -eq 1 ]]; then recovery_stop_services+=(bark-server); fi
  recovery_sql="SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname IN ('apollo', '$previous_db') AND pid <> pg_backend_pid(); ALTER DATABASE apollo RENAME TO $failed_restore_db; ALTER DATABASE $previous_db RENAME TO apollo;"
  recovery_validate_args=("$repo_dir/scripts/validate-deployment.sh" --env-file "$env_file" --public)
  if [[ $with_bark -eq 1 ]]; then recovery_validate_args+=(--with-bark); fi

  echo "manual commands to restore the retained pre-rollback database and current deployment:" >&2
  print_recovery_command "APOLLO_ENV_FILE=$env_file" "${compose[@]}" \
    up -d --no-deps --wait --wait-timeout 120 postgres
  print_recovery_command "APOLLO_ENV_FILE=$env_file" "${compose[@]}" \
    stop "${recovery_stop_services[@]}"
  print_recovery_command "APOLLO_ENV_FILE=$env_file" "${compose[@]}" exec -T postgres \
    psql -U apollo -d postgres -v ON_ERROR_STOP=1 -c "$recovery_sql"
  print_recovery_command "APOLLO_ENV_FILE=$env_file" "${compose[@]}" \
    up -d --no-build --remove-orphans --wait --wait-timeout 180
  print_recovery_command "COMPOSE_PROJECT_NAME=$project_name" "${recovery_validate_args[@]}"
  echo "the activated restored database must not be discarded until recovery is verified" >&2
  exit "$exit_status"
}

if [[ $restore_data -eq 1 ]]; then
  APOLLO_ENV_FILE="$env_file" "${compose[@]}" exec -T postgres \
    pg_restore --list < "$backup_dir/postgres.dump" >/dev/null

  safety_backup_root="${DEPLOYMENT_BACKUP_DIR:-/home/apollo/backups/apollo-backend}"
  safety_args=(
    --env-file "$env_file"
    --backup-dir "$safety_backup_root"
  )
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
  failed_restore_db="apollo_failed_restore_$restore_suffix"
  trap 'report_post_activation_failure "$?"' ERR

  if [[ $with_bark -eq 1 && -d "$backup_dir/barkdata" ]]; then
    APOLLO_ENV_FILE="$env_file" "${compose[@]}" stop bark-server
    bark_id="$(APOLLO_ENV_FILE="$env_file" "${compose[@]}" ps -a -q bark-server | head -n 1)"
    [[ -n "$bark_id" ]] && docker cp "$backup_dir/barkdata/." "$bark_id:/data/"
  fi
  echo "database restore verified before activation; prior database retained as $previous_db" >&2
  echo "current-state safety backup remains at $safety_backup" >&2
fi

APOLLO_ENV_FILE="$env_file" "${compose[@]}" stop
APOLLO_ENV_FILE="$env_file" "${rollback_compose[@]}" up -d --no-build --no-deps \
  --force-recreate --remove-orphans --wait --wait-timeout 120 "${rollback_running_services[@]}"

validate_args=(
  --env-file "$env_file"
  --public
  --expected-runtime-image-id "$captured_image_id"
  --expected-runtime-image-reference "$captured_image_reference"
)
if [[ $with_bark -eq 1 ]]; then validate_args+=(--with-bark); fi
COMPOSE_PROJECT_NAME="$project_name" "$repo_dir/scripts/validate-deployment.sh" "${validate_args[@]}"
trap - ERR
echo "captured deployment state restored from $backup_dir ($captured_image_reference)"

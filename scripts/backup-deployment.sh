#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"

env_file="$repo_dir/.env.docker"
with_bark=0
backup_root="${DEPLOYMENT_BACKUP_DIR:-/home/apollo/backups/apollo-backend}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env-file)
      [[ $# -ge 2 ]] || { echo "--env-file requires a path" >&2; exit 2; }
      env_file="$2"
      shift 2
      ;;
    --with-bark) with_bark=1; shift ;;
    --backup-dir)
      [[ $# -ge 2 ]] || { echo "--backup-dir requires a path" >&2; exit 2; }
      backup_root="$2"
      shift 2
      ;;
    -h|--help)
      echo "usage: $0 [--env-file PATH] [--with-bark] [--backup-dir PATH]"
      exit 0
      ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[[ -f "$env_file" ]] || { echo "missing environment file: $env_file" >&2; exit 1; }
for command in docker find git mktemp mv python3 readlink rm sha256sum sort xargs; do
  command -v "$command" >/dev/null || { echo "missing required command: $command" >&2; exit 1; }
done
env_file="$(readlink -f -- "$env_file")"
mkdir -p "$backup_root"
backup_root="$(readlink -f -- "$backup_root")"
chmod 700 "$backup_root"

compose=(docker compose --env-file "$env_file")
if [[ $with_bark -eq 1 ]]; then compose+=(--profile bark); fi

backup_dir=""
checksum_tmp=""
bark_restart_needed=0
cleanup_and_exit() {
  local exit_status="$1"
  trap - EXIT HUP INT TERM

  if [[ $bark_restart_needed -eq 1 ]]; then
    APOLLO_ENV_FILE="$env_file" "${compose[@]}" start bark-server >/dev/null 2>&1 || true
  fi
  if [[ -n "$checksum_tmp" ]]; then
    rm -f -- "$checksum_tmp" || true
  fi
  if [[ -n "$backup_dir" && -d "$backup_dir" ]]; then
    case "$backup_dir" in
      "$backup_root"/.*.partial.*) rm -rf -- "$backup_dir" || true ;;
      *) echo "refusing to remove unexpected backup staging path: $backup_dir" >&2 ;;
    esac
  fi
  exit "$exit_status"
}
trap 'cleanup_and_exit $?' EXIT
trap 'cleanup_and_exit 129' HUP
trap 'cleanup_and_exit 130' INT
trap 'cleanup_and_exit 143' TERM

app_services=(
  api scheduler worker-notifications worker-stuck-notifications
  worker-subreddits worker-trending worker-users worker-live-activities
)
deployment_image_label="com.apollo-reborn.deployment.image-reference"
apollo_image_reference=""
apollo_image_id=""
for service in "${app_services[@]}"; do
  service_found=0
  while IFS= read -r container_id; do
    [[ -n "$container_id" ]] || continue
    service_found=1
    running_image_id="$(docker inspect --format '{{.Image}}' "$container_id")"
    running_image_reference="$(docker inspect --format '{{.Config.Image}}' "$container_id")"
    running_image_label="$(docker inspect --format \
      '{{if .Config.Labels}}{{index .Config.Labels "com.apollo-reborn.deployment.image-reference"}}{{end}}' \
      "$container_id")"
    [[ "$running_image_id" =~ ^sha256:[0-9a-f]{64}$ ]] || {
      echo "cannot back up rollback state: $service has an invalid image ID" >&2
      exit 1
    }
    [[ "$(docker inspect --format '{{.State.Running}}' "$container_id")" == "true" ]] || {
      echo "cannot back up rollback state: $service is not running" >&2
      exit 1
    }

    captured_image_reference="$running_image_reference"
    if [[ "$running_image_reference" == "$running_image_id" ]]; then
      captured_image_reference="$running_image_label"
    elif [[ "$running_image_reference" =~ ^ghcr\.io/[a-z0-9._-]+/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$ ]]; then
      if [[ -n "$running_image_label" &&
            "$running_image_label" != "$running_image_reference" ]]; then
        echo "cannot back up rollback state: $service has conflicting image provenance" >&2
        exit 1
      fi
    else
      echo "cannot back up rollback state: $service lacks its immutable deployment reference" >&2
      exit 1
    fi

    [[ "$captured_image_reference" =~ ^ghcr\.io/[a-z0-9._-]+/[a-z0-9._/-]+@sha256:[0-9a-f]{64}$ ]] || {
      echo "cannot back up rollback state: $service lacks a valid immutable image reference" >&2
      exit 1
    }

    if [[ "$running_image_reference" == "$captured_image_reference" ]]; then
      reference_image_id="$(docker image inspect --format '{{.Id}}' "$captured_image_reference" 2>/dev/null)" || {
        echo "cannot back up rollback state: $service image reference is not present locally" >&2
        exit 1
      }
      [[ "$reference_image_id" == "$running_image_id" ]] || {
        echo "cannot back up rollback state: $service image reference and ID differ" >&2
        exit 1
      }
    fi

    if [[ -z "$apollo_image_reference" ]]; then
      apollo_image_reference="$captured_image_reference"
      apollo_image_id="$running_image_id"
    elif [[ "$captured_image_reference" != "$apollo_image_reference" ||
            "$running_image_id" != "$apollo_image_id" ]]; then
      echo "cannot back up rollback state: application image provenance differs" >&2
      exit 1
    fi
  done < <(APOLLO_ENV_FILE="$env_file" "${compose[@]}" ps -a -q "$service" 2>/dev/null)

  [[ $service_found -eq 1 ]] || {
    echo "cannot back up rollback state: $service container is missing" >&2
    exit 1
  }
done
[[ "$(docker image inspect --format '{{.Id}}' "$apollo_image_id" 2>/dev/null)" == "$apollo_image_id" ]] || {
  echo "cannot back up rollback state: application image ID is not present locally" >&2
  exit 1
}

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
final_backup_dir="$backup_root/$timestamp"
[[ ! -e "$final_backup_dir" ]] || {
  echo "backup destination already exists: $final_backup_dir" >&2
  exit 1
}
backup_dir="$(mktemp -d "$backup_root/.${timestamp}.partial.XXXXXX")"
chmod 700 "$backup_dir"

project_name="$(APOLLO_ENV_FILE="$env_file" "${compose[@]}" config --format json | python3 -c '
import json, sys
print(json.load(sys.stdin)["name"])
')"
[[ "$project_name" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || {
  echo "refusing unsafe Compose project name" >&2
  exit 1
}

APOLLO_ENV_FILE="$env_file" "${compose[@]}" exec -T postgres \
  pg_dump -U apollo -d apollo --format=custom > "$backup_dir/postgres.dump"

cp docker-compose.yml "$backup_dir/docker-compose.yml"
mkdir -p "$backup_dir/docs/deployment" "$backup_dir/docker" "$backup_dir/migrations"
cp docs/deployment/nginx.conf "$backup_dir/docs/deployment/nginx.conf"
cp docs/schema.sql "$backup_dir/docs/schema.sql"
cp docker/migrate.sh "$backup_dir/docker/"
cp migrations/000013_restore_live_activities.up.sql \
  migrations/000014_add_device_transport.up.sql "$backup_dir/migrations/"
ln -s "$repo_dir/secrets" "$backup_dir/secrets"
git rev-parse HEAD > "$backup_dir/git-revision.txt"
sha256sum "$env_file" | awk '{print $1}' > "$backup_dir/environment.sha256"
printf '%s\n' "$project_name" > "$backup_dir/project-name.txt"
printf '%s\n' "$apollo_image_reference" > "$backup_dir/apollo-image-reference.txt"
printf '%s\n' "$apollo_image_id" > "$backup_dir/apollo-image-id.txt"

image_ids=()
seen_image_ids=" "
{
  echo 'services:'
  while IFS= read -r service; do
    container_id="$(APOLLO_ENV_FILE="$env_file" "${compose[@]}" ps -a -q "$service" 2>/dev/null | head -n 1)"
    [[ -n "$container_id" ]] || continue
    image_id="$(docker inspect --format '{{.Image}}' "$container_id")"
    printf '  %s:\n    image: "%s"\n    pull_policy: never\n    build: !reset null\n' "$service" "$image_id"
    for app_service in "${app_services[@]}"; do
      if [[ "$service" == "$app_service" ]]; then
        printf '    labels:\n      %s: "%s"\n' \
          "$deployment_image_label" "$apollo_image_reference"
        break
      fi
    done
    printf '%s\n' "$service" >> "$backup_dir/rollback-services.txt"
    if [[ "$(docker inspect --format '{{.State.Running}}' "$container_id")" == "true" ]]; then
      printf '%s\n' "$service" >> "$backup_dir/rollback-running-services.txt"
    fi
    if [[ "$seen_image_ids" != *" $image_id "* ]]; then
      image_ids+=("$image_id")
      seen_image_ids+="$image_id "
    fi
  done < <(APOLLO_ENV_FILE="$env_file" "${compose[@]}" config --services)
} > "$backup_dir/rollback-images.yml"
[[ ${#image_ids[@]} -gt 0 ]] || { echo "no service images found" >&2; exit 1; }
[[ -s "$backup_dir/rollback-running-services.txt" ]] || {
  echo "no running services found" >&2
  exit 1
}
docker image save --output "$backup_dir/images.tar" "${image_ids[@]}"

if [[ $with_bark -eq 1 ]]; then
  bark_id="$(APOLLO_ENV_FILE="$env_file" "${compose[@]}" ps -q bark-server 2>/dev/null | head -n 1)"
  if [[ -n "$bark_id" ]]; then
    APOLLO_ENV_FILE="$env_file" "${compose[@]}" stop bark-server >/dev/null
    bark_restart_needed=1
    mkdir -p "$backup_dir/barkdata"
    docker cp "$bark_id:/data/." "$backup_dir/barkdata/"
    APOLLO_ENV_FILE="$env_file" "${compose[@]}" start bark-server >/dev/null
    bark_restart_needed=0
  fi
fi

find "$backup_dir" -type d -exec chmod 700 {} +
find "$backup_dir" -type f -exec chmod 600 {} +
chmod 644 \
  "$backup_dir/docs/deployment/nginx.conf" \
  "$backup_dir/docs/schema.sql" \
  "$backup_dir/migrations/000013_restore_live_activities.up.sql" \
  "$backup_dir/migrations/000014_add_device_transport.up.sql"
chmod 755 "$backup_dir/docker/migrate.sh"
checksum_tmp="$(mktemp "$backup_root/.apollo-sha256.XXXXXX")"
if ! (
  cd "$backup_dir"
  find . -type f ! -name SHA256SUMS -print0 |
    sort -z |
    xargs -0 sha256sum
) > "$checksum_tmp"; then
  rm -f "$checksum_tmp"
  exit 1
fi
mv -T -- "$checksum_tmp" "$backup_dir/SHA256SUMS"
checksum_tmp=""
chmod 600 "$backup_dir/SHA256SUMS"

mv -T -- "$backup_dir" "$final_backup_dir"
backup_dir=""
echo "$final_backup_dir"

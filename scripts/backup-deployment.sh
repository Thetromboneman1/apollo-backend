#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"

env_file="$repo_dir/.env.docker"
with_bark=0
backup_root="${DEPLOYMENT_BACKUP_DIR:-$(dirname "$repo_dir")/apollo-backend-backups}"

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
for command in docker find git python3 sha256sum sort xargs; do
  command -v "$command" >/dev/null || { echo "missing required command: $command" >&2; exit 1; }
done
compose=(docker compose --env-file "$env_file")
if [[ $with_bark -eq 1 ]]; then compose+=(--profile bark); fi

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
backup_dir="$backup_root/$timestamp"
mkdir -p "$backup_root"
chmod 700 "$backup_root"
mkdir "$backup_dir"
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

image_ids=()
seen_image_ids=" "
{
  echo 'services:'
  while IFS= read -r service; do
    container_id="$(APOLLO_ENV_FILE="$env_file" "${compose[@]}" ps -a -q "$service" 2>/dev/null | head -n 1)"
    [[ -n "$container_id" ]] || continue
    image_id="$(docker inspect --format '{{.Image}}' "$container_id")"
    printf '  %s:\n    image: "%s"\n    pull_policy: never\n    build: !reset null\n' "$service" "$image_id"
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

bark_restart_needed=0
restart_bark_on_exit() {
  if [[ $bark_restart_needed -eq 1 ]]; then
    APOLLO_ENV_FILE="$env_file" "${compose[@]}" start bark-server >/dev/null 2>&1 || true
  fi
}
trap restart_bark_on_exit EXIT
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
chmod 700 "$backup_dir/docker/migrate.sh"
(
  cd "$backup_dir"
  find . -type f ! -name SHA256SUMS -print0 |
    sort -z |
    xargs -0 sha256sum > SHA256SUMS
)
chmod 600 "$backup_dir/SHA256SUMS"

echo "$backup_dir"

#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_dir"

env_file="$repo_dir/.env.docker"
with_bark=0
public_check=0
config_only=0

usage() {
  echo "usage: $0 [--env-file PATH] [--with-bark] [--public] [--config-only]" >&2
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --env-file)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      env_file="$2"
      shift 2
      ;;
    --with-bark) with_bark=1; shift ;;
    --public) public_check=1; shift ;;
    --config-only) config_only=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) usage; exit 2 ;;
  esac
done

[[ -f "$env_file" ]] || { echo "missing environment file: $env_file" >&2; exit 1; }

if env_mode="$(stat -f '%Lp' "$env_file" 2>/dev/null)"; then
  :
else
  env_mode="$(stat -c '%a' "$env_file")"
fi
env_mode_number=$((8#$env_mode))
if (( (env_mode_number & 8#077) != 0 )); then
  echo "environment file permissions must not grant group or other access: $env_mode" >&2
  exit 1
fi

read_env() {
  awk -v wanted="$1" '
    index($0, wanted "=") == 1 {
      sub(/^[^=]*=/, "")
      sub(/\r$/, "")
      print
      exit
    }
  ' "$env_file"
}

apollo_image="$(read_env APOLLO_IMAGE)"
if [[ ! "$apollo_image" =~ ^[^@[:space:]]+@sha256:[0-9a-fA-F]{64}$ ]]; then
  echo "APOLLO_IMAGE must be an immutable sha256 digest reference" >&2
  exit 1
fi

postgres_password="$(read_env POSTGRES_PASSWORD)"
if [[ ${#postgres_password} -lt 32 || "$postgres_password" == REPLACE_* ]]; then
  echo "POSTGRES_PASSWORD must be a non-placeholder value of at least 32 characters" >&2
  exit 1
fi
if [[ ! "$postgres_password" =~ ^[A-Za-z0-9._~+-]+$ ]]; then
  echo "POSTGRES_PASSWORD must contain only URL-safe characters" >&2
  exit 1
fi

registration_secret="$(read_env REGISTRATION_SECRET)"
if [[ ${#registration_secret} -lt 32 || "$registration_secret" == REPLACE_* ]]; then
  echo "REGISTRATION_SECRET must be a non-placeholder value of at least 32 characters" >&2
  exit 1
fi
if [[ ! "$registration_secret" =~ ^[A-Za-z0-9._~+-]+$ ]]; then
  echo "REGISTRATION_SECRET must contain only A-Z, a-z, 0-9, dot, underscore, tilde, plus, or hyphen" >&2
  exit 1
fi

bark_allowed_origins="$(read_env BARK_ALLOWED_ORIGINS)"
[[ -n "$bark_allowed_origins" ]] || { echo "BARK_ALLOWED_ORIGINS must not be empty" >&2; exit 1; }

public_url="$(read_env PUBLIC_URL)"
[[ -n "$public_url" ]] || public_url="https://apollo.connerclan.com"
[[ "$public_url" == "https://apollo.connerclan.com" ]] || {
  echo "PUBLIC_URL must be https://apollo.connerclan.com for this deployment" >&2
  exit 1
}

for command in docker python3; do
  command -v "$command" >/dev/null || { echo "missing required command: $command" >&2; exit 1; }
done

compose=(docker compose --env-file "$env_file")
if [[ $with_bark -eq 1 ]]; then
  compose+=(--profile bark)
fi

APOLLO_ENV_FILE="$env_file" "${compose[@]}" config --quiet

# Inspect the rendered model without printing it: it contains environment
# values. Every host publication must be explicitly loopback-bound, and the Go
# API itself must have no host publication.
APOLLO_ENV_FILE="$env_file" "${compose[@]}" config --format json | python3 -c '
import json, sys
cfg = json.load(sys.stdin)
expected_image = sys.argv[1]
bad = []
for service, definition in cfg.get("services", {}).items():
    for port in definition.get("ports") or []:
        host_ip = port.get("host_ip") or ""
        if host_ip != "127.0.0.1":
            bad.append("{}:{}".format(service, port.get("target")))
if (cfg.get("services", {}).get("api", {}).get("ports") or []):
    bad.append("api must not publish a host port")
if bad:
    raise SystemExit("non-loopback publication: " + ", ".join(bad))
required_restart = {
    name for name in cfg.get("services", {})
    if name != "migrate"
}
missing = sorted(
    name for name in required_restart
    if cfg["services"][name].get("restart") != "unless-stopped"
)
if missing:
    raise SystemExit("services without restart: unless-stopped: " + ", ".join(missing))
app_services = {
    "api", "scheduler", "worker-notifications", "worker-stuck-notifications",
    "worker-subreddits", "worker-trending", "worker-users", "worker-live-activities",
}
for name in app_services:
    service = cfg.get("services", {}).get(name) or {}
    if service.get("image") != expected_image:
        raise SystemExit(name + " is not pinned to APOLLO_IMAGE")
    if service.get("build"):
        raise SystemExit(name + " unexpectedly has a build definition")
    if not service.get("read_only"):
        raise SystemExit(name + " root filesystem is not read-only")
    if "ALL" not in (service.get("cap_drop") or []):
        raise SystemExit(name + " does not drop all capabilities")
for name in ("redis-queue", "redis-locks"):
    service = cfg.get("services", {}).get(name) or {}
    command = " ".join(str(value) for value in service.get("command") or [])
    if "--appendonly yes" not in command or "--maxmemory-policy noeviction" not in command:
        raise SystemExit(name + " persistence or noeviction policy is missing")
' "$apollo_image"

if [[ $config_only -eq 1 ]]; then
  echo "configuration validation passed"
  exit 0
fi

for command in curl mktemp dd; do
  command -v "$command" >/dev/null || { echo "missing required command: $command" >&2; exit 1; }
done

local_url="http://127.0.0.1:4000"

health="$(curl --fail --silent --show-error --max-time 10 "$local_url/v1/health")"
[[ "$health" == *'"status":"available"'* ]] || { echo "unexpected local health response" >&2; exit 1; }

unauth_code="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --max-time 10 --request POST --header 'Content-Type: application/json' \
  --data '{}' "$local_url/v1/device")"
[[ "$unauth_code" == "401" ]] || {
  echo "registration authentication failed closed check: expected 401, got $unauth_code" >&2
  exit 1
}
wrong_token_code="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}'   --max-time 10 --request POST --header 'Content-Type: application/json'   --header 'X-Registration-Token: deliberately-wrong' --data '{}' "$local_url/v1/device")"
[[ "$wrong_token_code" == "401" ]] || {
  echo "wrong registration credential check: expected 401, got $wrong_token_code" >&2
  exit 1
}

header_file="$(mktemp "${TMPDIR:-/tmp}/apollo-header.XXXXXX")"
oversize_file="$(mktemp "${TMPDIR:-/tmp}/apollo-oversize.XXXXXX")"
cleanup() {
  rm -f "$header_file" "$oversize_file"
}
trap cleanup EXIT
chmod 600 "$header_file" "$oversize_file"
printf 'X-Registration-Token: %s\n' "$registration_secret" > "$header_file"

auth_code="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --max-time 10 --request POST --header 'Content-Type: application/json' \
  --header "@$header_file" --data '{}' "$local_url/v1/device")"
[[ "$auth_code" == "422" ]] || {
  echo "registration credential acceptance check: expected safe validation failure 422, got $auth_code" >&2
  exit 1
}

dd if=/dev/zero of="$oversize_file" bs=1048577 count=1 2>/dev/null
oversize_code="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
  --max-time 15 --request POST --header 'Content-Type: application/octet-stream' \
  --data-binary "@$oversize_file" "$local_url/api/req_v2")"
[[ "$oversize_code" == "413" ]] || {
  echo "oversize request check: expected 413, got $oversize_code" >&2
  exit 1
}

gateway_port="$(APOLLO_ENV_FILE="$env_file" "${compose[@]}" port origin-gateway 8080)"
[[ "$gateway_port" == "127.0.0.1:4000" ]] || {
  echo "origin isolation check failed: origin-gateway is published as $gateway_port" >&2
  exit 1
}
api_container_id="$(APOLLO_ENV_FILE="$env_file" "${compose[@]}" ps -q api)"
[[ -n "$api_container_id" ]] || { echo "origin isolation check failed: api container is missing" >&2; exit 1; }
docker inspect "$api_container_id" | python3 -c '
import json, sys
container = json.load(sys.stdin)[0]
bindings = (container.get("HostConfig", {}).get("PortBindings") or {}).get("4000/tcp") or []
if bindings:
    raise SystemExit("origin isolation check failed: api has a host publication")
'

# Compose records the requested immutable reference on each container, while
# Docker records the exact local image object used to create it. Require both
# to match so a stale or manually substituted application image cannot pass
# validation merely because the rendered Compose model is correct.
expected_image_id="$(docker image inspect --format '{{.Id}}' "$apollo_image")"
[[ -n "$expected_image_id" ]] || { echo "pinned application image is not present locally" >&2; exit 1; }
for service in api scheduler worker-notifications worker-stuck-notifications \
  worker-subreddits worker-trending worker-users worker-live-activities; do
  container_id="$(APOLLO_ENV_FILE="$env_file" "${compose[@]}" ps -q "$service")"
  [[ -n "$container_id" ]] || { echo "$service container is missing" >&2; exit 1; }
  running_image_ref="$(docker inspect --format '{{.Config.Image}}' "$container_id")"
  running_image_id="$(docker inspect --format '{{.Image}}' "$container_id")"
  if [[ "$running_image_ref" != "$apollo_image" || "$running_image_id" != "$expected_image_id" ]]; then
    echo "$service is not running the pinned APOLLO_IMAGE" >&2
    exit 1
  fi
done

# Bark URLs contain bearer device keys. Feed them directly to the parser and
# print only a disallowed normalized origin, never the complete URL.
if APOLLO_ENV_FILE="$env_file" "${compose[@]}" ps --status running --services | grep -qx postgres; then
  APOLLO_ENV_FILE="$env_file" "${compose[@]}" exec -T postgres \
    psql -U apollo -d apollo -Atq -c \
    "SELECT transport_endpoint FROM devices WHERE transport = 'bark' AND transport_endpoint <> '';" | \
    python3 -c '
import sys
from urllib.parse import urlsplit

def normalized_origin(raw, allow_endpoint_path):
    try:
        parsed = urlsplit(raw.strip())
        port = parsed.port
    except ValueError as exc:
        raise ValueError("invalid URL or port") from exc
    if parsed.scheme not in {"http", "https"}:
        raise ValueError("scheme must be http or https")
    if parsed.username is not None or parsed.password is not None:
        raise ValueError("userinfo is not allowed")
    host = (parsed.hostname or "").lower().rstrip(".")
    if not host:
        raise ValueError("host is required")
    if port is None:
        port = 443 if parsed.scheme == "https" else 80
    if port not in {80, 443, 8080}:
        raise ValueError("unsafe port")
    if not allow_endpoint_path and (parsed.path not in {"", "/"} or parsed.query or parsed.fragment):
        raise ValueError("allowed origin must not include a path, query, or fragment")
    return parsed.scheme + "://" + host + ":" + str(port)

allowed = set()
for raw_origin in sys.argv[1].split(","):
    try:
        allowed.add(normalized_origin(raw_origin, False))
    except ValueError as exc:
        raise SystemExit("invalid BARK_ALLOWED_ORIGINS entry: " + str(exc))
failed = False
for raw in sys.stdin:
    try:
        origin = normalized_origin(raw, True)
    except ValueError:
        origin = "<invalid>"
    if origin not in allowed:
        print("disallowed persisted Bark origin: " + origin, file=sys.stderr)
        failed = True
if failed:
    raise SystemExit(1)
' "$bark_allowed_origins"
fi

if [[ $public_check -eq 1 ]]; then
  public_health="$(curl --fail --silent --show-error --max-time 15 "$public_url/v1/health")"
  [[ "$public_health" == *'"status":"available"'* ]] || { echo "unexpected public health response" >&2; exit 1; }

  public_unauth_code="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
    --max-time 15 --request POST --header 'Content-Type: application/json' \
    --data '{}' "$public_url/v1/device")"
  [[ "$public_unauth_code" == "401" ]] || {
    echo "public registration authentication check: expected 401, got $public_unauth_code" >&2
    exit 1
  }

  public_oversize_code="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
    --max-time 20 --request POST --header 'Content-Type: application/octet-stream' \
    --data-binary "@$oversize_file" "$public_url/api/req_v2")"
  [[ "$public_oversize_code" == "413" ]] || {
    echo "public oversize request check: expected 413, got $public_oversize_code" >&2
    exit 1
  }
fi

echo "deployment validation passed"

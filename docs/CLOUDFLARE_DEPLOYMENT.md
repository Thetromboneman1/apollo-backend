# ConnerClan Mini production deployment

This is the production path for **https://apollo.connerclan.com**.

Apollo does not run on the MacBook. The MacBook is only an SSH controller. Source, images, containers, databases, secrets, logs, backups, Cloudflare services, and deployment documentation stay on ConnerClan-Mini or its Debian VM.

## Production boundary

The Mini runs the VirtualBox VM named **Apollo Backend**. The VM runs Debian, Docker Engine, the backend stack, backups, and cloudflared.

The public path is:

~~~text
Apollo on iPhone
  -> https://apollo.connerclan.com
  -> Cloudflare Tunnel
  -> 127.0.0.1:4000 in the Debian VM
  -> Nginx origin gateway
  -> private Docker API network
~~~

Only **/v1/health** is unauthenticated. Every other backend route requires **X-Registration-Token**.

The origin gateway is the only published Compose port. It binds to **127.0.0.1:4000**, caps requests at 1 MiB, and enforces connection, rate, header, body, and proxy timeouts. Postgres, PgBouncer, Redis, the API, scheduler, and workers have no host ports.

## 1. Verify the VM

Run these commands inside the Debian VM:

~~~bash
hostname
docker --version
docker compose version
cloudflared --version
systemctl is-enabled docker
systemctl is-active docker
~~~

The expected host name is **apollo-vm**. Docker must be enabled and active.

The production checkout is:

~~~text
/home/apollo/src/apollo-backend
~~~

Do not deploy a dirty checkout.

## 2. Publish an immutable image

The release workflow builds **linux/amd64** and **linux/arm64**, publishes an SBOM and provenance, signs the image with GitHub OIDC, and uploads **APOLLO_IMAGE.txt**.

The deployment value must look like this:

~~~text
ghcr.io/thetromboneman1/apollo-backend@sha256:<64 lowercase hex characters>
~~~

A tag such as **latest**, **main**, or **sha-...** is not accepted by the deployment validator.

Before deploy, verify the workflow completed for the exact commit and verify its signature:

~~~bash
cosign verify   --certificate-oidc-issuer https://token.actions.githubusercontent.com   --certificate-identity-regexp '^https://github.com/Thetromboneman1/apollo-backend/.github/workflows/docker.yml@'   "$APOLLO_IMAGE"
~~~

## 3. Create the production environment

From the production checkout:

~~~bash
cp .env.docker.example .env.docker
chmod 600 .env.docker
mkdir -p secrets
chmod 700 secrets
~~~

Set these values:

- **APOLLO_IMAGE**: exact digest reference from the successful workflow.
- **POSTGRES_PASSWORD**: at least 32 URL-safe random characters.
- **REGISTRATION_SECRET**: at least 32 URL-safe random characters.
- **BARK_ALLOWED_ORIGINS=https://api.day.app**
- **PUBLIC_URL=https://apollo.connerclan.com**
- **ENV=production**

Generate passwords without placing their values in command history:

~~~bash
umask 077
secret_dir="$(mktemp -d)"
cleanup_secret_files() {
  rm -f -- "$secret_dir/database" "$secret_dir/registration"
  rmdir "$secret_dir"
}
trap cleanup_secret_files EXIT
openssl rand -hex 32 > "$secret_dir/database"
openssl rand -hex 32 > "$secret_dir/registration"
python3 - "$secret_dir" <<'PYENV'
from pathlib import Path
import sys

path = Path(".env.docker")
secret_dir = Path(sys.argv[1])
text = path.read_text()
text = text.replace(
    "REPLACE_WITH_A_RANDOM_DATABASE_PASSWORD_OF_AT_LEAST_32_CHARACTERS",
    (secret_dir / "database").read_text().strip(),
)
text = text.replace(
    "REPLACE_WITH_A_RANDOM_SECRET_OF_AT_LEAST_32_CHARACTERS",
    (secret_dir / "registration").read_text().strip(),
)
path.write_text(text)
PYENV
cleanup_secret_files
trap - EXIT
chmod 600 .env.docker
~~~

Do not print or paste either value. Store the registration token in 1Password and enter the same value in Apollo on the iPhone.

This deployment uses hosted Bark at **https://api.day.app**. It does not start the optional self-hosted Bark container. Leave the four APPLE values empty unless a verified APNs signing setup is added later.

## 4. Install the Cloudflare systemd service

The remotely managed tunnel is named **apollo-mac-mini**. Its public hostname must route to:

~~~text
http://127.0.0.1:4000
~~~

Do not put the tunnel UUID or connector token in Git, docs, command arguments, or shell history.

Create a locked service account and token directory:

~~~bash
sudo useradd --system --home /nonexistent --shell /usr/sbin/nologin cloudflared   2>/dev/null || true
sudo install -d -o cloudflared -g cloudflared -m 700 /etc/cloudflared
~~~

Install the connector token through a hidden prompt:

~~~bash
read -r -s -p 'Cloudflare tunnel token: ' tunnel_token
printf '
'
printf '%s' "$tunnel_token" |   sudo install -o cloudflared -g cloudflared -m 600 /dev/stdin   /etc/cloudflared/apollo.token
unset tunnel_token
~~~

Install and start the unit:

~~~bash
sudo install -o root -g root -m 644   docs/deployment/cloudflared-apollo.service   /etc/systemd/system/cloudflared-apollo.service
sudo systemctl daemon-reload
sudo systemctl enable --now cloudflared-apollo.service
systemctl is-enabled cloudflared-apollo.service
systemctl is-active cloudflared-apollo.service
~~~

The service reads **/etc/cloudflared/apollo.token** directly. The deployment script requires the unit to be enabled and active before it changes containers.

## 5. Validate and deploy

Validate the environment and rendered Compose model first:

~~~bash
scripts/validate-deployment.sh --config-only
~~~

Deploy:

~~~bash
scripts/deploy-cloudflare.sh --attempts 2
~~~

The runner:

- refuses a dirty checkout;
- refuses a tag-based application image;
- requires the tunnel systemd unit;
- pulls without building;
- takes a verified backup when a prior database is running;
- starts the stack with **--no-build**;
- checks local and public health;
- checks missing and wrong credentials return **401**;
- checks a valid credential reaches safe input validation;
- checks requests over 1 MiB return **413**;
- checks the API has no host port;
- checks every running application container uses the configured digest reference and exact local image ID;
- checks persisted Bark origins without printing bearer URLs;
- rolls back captured images after two failed attempts.

Run acceptance again at any time:

~~~bash
scripts/validate-deployment.sh --public
~~~

## 6. Install daily backups

Install the service and timer:

~~~bash
sudo install -o root -g root -m 644   docs/deployment/apollo-backup.service   /etc/systemd/system/apollo-backup.service
sudo install -o root -g root -m 644   docs/deployment/apollo-backup.timer   /etc/systemd/system/apollo-backup.timer
sudo systemctl daemon-reload
sudo systemctl enable --now apollo-backup.timer
systemctl list-timers apollo-backup.timer
~~~

Backups are written outside the checkout to:

~~~text
/home/apollo/backups/apollo-backend
~~~

Each backup includes a PostgreSQL custom dump, exact Git revision, environment hash, captured Compose inputs, running-service manifest, archived container images, and **SHA256SUMS**. The timer keeps 30 days by default.

Backups contain private application data. Keep the directory owner-only and replicate it to encrypted off-host storage.

## 7. Rollback limits

Restore captured images without changing data:

~~~bash
scripts/rollback-deployment.sh   /home/apollo/backups/apollo-backend/YYYYMMDDTHHMMSSZ   --confirm
~~~

Restore images and PostgreSQL data only when needed:

~~~bash
scripts/rollback-deployment.sh   /home/apollo/backups/apollo-backend/YYYYMMDDTHHMMSSZ   --confirm --restore-data
~~~

Rollback verifies **SHA256SUMS** before loading an image archive. Data restore is tested in a temporary database before activation.

The live **.env.docker** and **secrets** directory are intentionally not copied into backups and are not rolled back. The backup contains an owner-only same-host link to the live secrets directory and a hash of the environment used at capture time. Rollback warns when that hash changed and validates the public endpoint after restoration.

## 8. Reboot persistence

Three layers must come back:

1. VirtualBox starts the **Apollo Backend** VM on the Mini.
2. Docker and **cloudflared-apollo.service** start in the Debian VM.
3. Compose restarts every persistent service with **restart: unless-stopped**.

After a reboot:

~~~bash
systemctl is-enabled docker cloudflared-apollo.service
systemctl is-active docker cloudflared-apollo.service
docker compose --env-file .env.docker ps
curl --fail http://127.0.0.1:4000/v1/health
curl --fail https://apollo.connerclan.com/v1/health
~~~

The restart policy does not restart a container that was deliberately stopped.

## 9. iPhone settings

Install Bark from the App Store and allow notifications.

In Apollo, open **Settings > Custom API > Notification Backend** and set:

- Backend URL: **https://apollo.connerclan.com**
- Registration Token: the production **REGISTRATION_SECRET**
- Bark Delivery: on
- Bark Push URL: the URL shown by the Bark app for **https://api.day.app**

Tap **Test Connection**, then send a test Bark notification.

Do not paste the registration token or Bark URL into logs or chat. Both are bearer capabilities.

## Acceptance checklist

- No Apollo source, build output, runtime, secret, or deployment document exists on the MacBook.
- The backend checkout is clean and pushed.
- CI passed for the exact commit.
- APOLLO_IMAGE is digest-pinned and its keyless signature verifies.
- All persistent containers are healthy or running.
- The origin gateway is exactly **127.0.0.1:4000**.
- The API has no host port.
- The tunnel and Docker services are enabled and active.
- Local and public health pass.
- Missing and wrong credentials return **401**.
- Oversize requests return **413**.
- A backup verifies and a restore rehearsal succeeds.
- The Mini reboot brings the VM, tunnel, and stack back without a manual login.

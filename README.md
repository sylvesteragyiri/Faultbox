# Faultbox
Lightweight, self-hosted error and queue monitoring for Laravel applications.

> **Status:** early development.
> - **M1:** the dashboard, projects, API keys and health probes.
> - **M2:** ingestion. Exceptions and failed jobs are received, grouped into issues and stored.
> - **M3:** the Laravel package, [`faultbox/laravel`](https://github.com/sylvesteragyiri/faultbox-laravel).
> - **M4:** the issues dashboard: lists, filters, search, issue detail, and resolve/ignore/reopen.
> - **M5:** the queue failures dashboard, failed-job list and detail.
> - **M6:** email and webhook notifications.
> - **Next:** operations and hardening (M7).
>
> See [docs/roadmap.md](docs/roadmap.md).

## Run with Docker Compose

[compose.yaml](compose.yaml) builds the image from this repository and runs it inside the 128 MB / 0.5 CPU budget, with the database on a named volume.

1. Create a `.env` next to `compose.yaml`:

   ```env
   FAULTBOX_SECRET=paste-the-output-of: openssl rand -base64 48
   FAULTBOX_ADMIN_EMAIL=you@example.com
   FAULTBOX_ADMIN_PASSWORD=a-long-password
   FAULTBOX_PUBLIC_URL=http://localhost:8080
   ```

   Keep `FAULTBOX_SECRET` the same forever. It signs CSRF tokens and encrypts stored notification secrets, so changing it makes saved SMTP passwords and webhook secrets unreadable. If you already ran `scripts/dev.ps1` or `scripts/dev.sh`, the `.env` they created works as is.

2. Start it:

   ```bash
   docker compose up -d --build
   docker compose ps          # wait for "healthy"
   docker compose logs -f     # JSON logs
   ```

3. Open http://localhost:8080 and log in. Go to **Projects → New project**. Copy the API key: it is shown only once.

Other settings:
- **A different port:** `FAULTBOX_HOST_PORT=9000`.
- **The published image instead of a local build:** swap `build:` for `image: ghcr.io/sylvesteragyiri/faultbox:edge` in `compose.yaml`.

The data lives in the `faultbox-data` volume. `docker compose down` keeps it; `docker compose down -v` deletes it.

Without Compose, the same thing as one command:

```bash
docker run -d --name faultbox -p 8080:8080   -v faultbox-data:/data   -e FAULTBOX_SECRET="$(openssl rand -base64 48)"   -e FAULTBOX_ADMIN_EMAIL=you@example.com   -e FAULTBOX_ADMIN_PASSWORD='a-long-password'   --memory 128m --cpus 0.5   ghcr.io/sylvesteragyiri/faultbox:edge
```

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `FAULTBOX_SECRET` | **required** | At least 32 bytes. Keys for CSRF tokens and for encrypting stored notification secrets are derived from it. Never change it once set. |
| `FAULTBOX_PORT` | `8080` | HTTP port. |
| `FAULTBOX_DATABASE_PATH` | `faultbox.db` (`/data/faultbox.db` in Docker) | SQLite file. Created and migrated on startup. |
| `FAULTBOX_ADMIN_EMAIL` / `FAULTBOX_ADMIN_PASSWORD` | — | Create the first dashboard user, but only while no users exist. The password must be at least 12 characters. |
| `FAULTBOX_PUBLIC_URL` | taken from the request | External URL, shown as the DSN in Laravel setup snippets and used for links in alert emails and webhooks. |
| `FAULTBOX_TRUST_PROXY` | `false` | Trust `X-Forwarded-For` and `X-Forwarded-Proto`. Enable only behind your own reverse proxy. |
| `FAULTBOX_RETENTION_DAYS` | `30` | Event retention. Used from M7. |
| `FAULTBOX_MAX_EVENT_KB` | `256` | Largest event body after decompression. |
| `FAULTBOX_RATE_LIMIT` / `FAULTBOX_RATE_BURST` | `50` / `200` | Events per second per project, and the burst size. |
| `FAULTBOX_QUEUE_SIZE` / `FAULTBOX_QUEUE_MAX_MB` | `1000` / `32` | Ingestion queue limits. When either is reached, the API answers `503`. |
| `FAULTBOX_SHUTDOWN_TIMEOUT` | `10s` | Grace period for in-flight requests on shutdown. |
| `FAULTBOX_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. Logs are JSON on stdout. |

## Commands

```bash
faultbox                                      # run the server
faultbox healthcheck                          # exit 0 if /ready is OK (used by Docker HEALTHCHECK)
echo 'password' | faultbox user create --email you@example.com
faultbox version
```

## Connect a Laravel app

The SDK is [`faultbox/laravel`](https://github.com/sylvesteragyiri/faultbox-laravel), for Laravel 12 and 13 on PHP 8.2 or later.

### 1. Install the package

It isn't on Packagist yet, so add it as a Composer repository first.

**From a local checkout** (the `faultbox-laravel` folder next to your app):

```bash
composer config repositories.faultbox path ../faultbox-laravel
composer require faultbox/laravel:@dev
```

**From Git**, once the repository is pushed:

```bash
composer config repositories.faultbox vcs https://github.com/sylvesteragyiri/faultbox-laravel
composer require faultbox/laravel:dev-main
```

When it's published, plain `composer require faultbox/laravel` is enough. Package auto-discovery registers everything, so no provider needs adding.

> **Docker note for path repositories:** Composer symlinks the package. If your app runs in a container, the `faultbox-laravel` folder must also be mounted inside it, at the same relative path, or the symlink breaks. Either mount the parent folder, or use the Git option.

### 2. Point it at Faultbox

Add to the app's `.env`:

```env
FAULTBOX_DSN=http://localhost:8080
FAULTBOX_API_KEY=fbx_...                 # from Projects → New project
FAULTBOX_RELEASE=1.4.2                   # optional: git tag or commit SHA
# FAULTBOX_ENVIRONMENT=production        # defaults to APP_ENV
```

`FAULTBOX_DSN` is the address **the Laravel app** uses to reach Faultbox, which depends on where each one runs:

| Laravel runs | Faultbox runs | `FAULTBOX_DSN` |
|---|---|---|
| On your machine (`php artisan serve`, Herd, Valet) | Compose on the same machine | `http://localhost:8080` |
| In Docker (Sail or its own Compose project) | Compose on the same machine | `http://faultbox:8080`, after joining the `faultbox` network (below) |
| In Docker, without joining the network | Compose on the same machine | `http://host.docker.internal:8080` (on Linux, add `extra_hosts: ["host.docker.internal:host-gateway"]`) |
| A production server | Behind your reverse proxy | `https://errors.example.com` (also set `FAULTBOX_PUBLIC_URL` and `FAULTBOX_TRUST_PROXY=true` on Faultbox) |

To join the network from the app's `docker-compose.yml` (for Sail, the `laravel.test` service):

```yaml
services:
  laravel.test:          # your app service
    networks:
      - sail             # keep your existing network
      - faultbox
networks:
  faultbox:
    external: true       # created by Faultbox's compose.yaml
```

### 3. Check it

```bash
php artisan config:clear
php artisan faultbox:test
```

This sends a test exception. If it fails, it says why: the server can't be reached (check `FAULTBOX_DSN`), the key was rejected, the project is disabled, or the event was refused (for example, an environment the project doesn't allow). Then trigger a real error and watch it appear under **Issues**. Failed queue jobs appear under **Queue Failures** with no changes to your jobs.

To get alerts, add a channel under **Notifications**.

### Will it slow down or break my app?

No, by design:
- **Only on errors:** the SDK makes no network calls unless it reports an exception or failed job, so healthy requests pay nothing.
- **After the response:** in web requests, events are sent after the response has been sent.
- **Errors swallowed:** every hook is wrapped in a `try/catch`.
- **Bounded outages:** strict timeouts, a 3 s budget per request or job, and a circuit breaker mean a Faultbox outage costs your app at most one short timeout per minute per worker.
- **Kill switch:** `FAULTBOX_ENABLED=false` turns it off completely.

The package README's [Safety section](https://github.com/sylvesteragyiri/faultbox-laravel#safety-it-never-slows-down-or-breaks-your-app) has the details, worst-case numbers and the tests behind them.

### Laravel Octane

Octane is supported. Under Octane one worker serves many requests, so the SDK listens to Octane's `RequestReceived` and `RequestTerminated` events:
- it resets the context, user and correlation ID at the start of each request; context set in a service provider is kept;
- it reads the request and the logged-in user from that request's sandbox;
- it sends events after the response.

There's nothing to configure. After installing or updating the package, restart the workers so they load it:

```bash
php artisan octane:reload
```

Queue workers (`queue:work`, Horizon) are separate processes and also need a restart (`php artisan queue:restart` or `horizon:terminate`).

See the package README for queue options, manual reporting, context and `Faultbox::fake()` in tests.

## HTTP endpoints

| Endpoint | Auth | Purpose |
|---|---|---|
| `GET /health` | none | Liveness: the process is serving HTTP. |
| `GET /ready` | none | Readiness: the database answers and the ingestion queue has room. |
| `POST /api/v1/events` | project API key | Report an exception or a message. Answers `202` right away; the event is stored a moment later. |
| `POST /api/v1/queue-failures` | project API key | Report a failed queue job. |
| `POST /api/v1/notifications` | project API key | Report a batch of Laravel notifications, sent or failed. A retried batch (same `batch_id`) is counted once. |
| `GET /api/v1/projects/me` | project API key | Shows which project a key belongs to. |

Send the key as `Authorization: Bearer fbx_…` or in the `X-Faultbox-Key` header.

API errors always use one JSON shape: `{"error": {"code": "invalid_api_key", "message": "…"}}`. The full contract, with JSON Schemas, limits and grouping rules, is in [docs/api/](docs/api/README.md).

## Run locally

The dev scripts create a git-ignored `.env` on first run, with a random `FAULTBOX_SECRET` and admin password. They print the login, then start the server on http://localhost:8080.

```powershell
# Windows (PowerShell)
.\scripts\dev.ps1
# if scripts are blocked: powershell -ExecutionPolicy Bypass -File .\scripts\dev.ps1
```

```bash
# Linux / macOS
./scripts/dev.sh        # or: sh scripts/dev.sh
```

- **Overriding settings:** variables already set in your shell take priority over `.env`.
- **Passing arguments:** anything after the script name goes to `faultbox`. For example, `echo 'a-long-password' | ./scripts/dev.sh user create --email me@example.com`.
- **The database** is stored at `data/faultbox.db`.
- **Starting over:** delete `data/` to reset the database, and `.env` to generate new credentials.
- **Settings reference:** [.env.example](.env.example) lists every setting.

## Development

```bash
go test ./...                # see tests/README.md for the suites
golangci-lint run ./...
```

## License

Apache-2.0. See [LICENSE](LICENSE).

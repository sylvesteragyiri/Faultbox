# Faultbox
Lightweight, self-hosted error and queue monitoring for Laravel applications.

> **Status:** early development.
> - **M1:** the dashboard, projects, API keys and health probes.
> - **M2:** ingestion. Exceptions and failed jobs are received, grouped into issues and stored.
> - **Next:** the Laravel package (M3) and the issue views (M4).
>
> See [docs/roadmap.md](docs/roadmap.md).

## Run with Docker

```bash
docker run -d --name faultbox -p 8080:8080 \
  -v faultbox-data:/data \
  -e FAULTBOX_SECRET="$(openssl rand -base64 48)" \
  -e FAULTBOX_ADMIN_EMAIL=you@example.com \
  -e FAULTBOX_ADMIN_PASSWORD='a-long-password' \
  --memory 128m --cpus 0.5 \
  ghcr.io/sylvesteragyiri/faultbox:edge
```

Then open http://localhost:8080 and log in. Keep `FAULTBOX_SECRET` the same across restarts: it signs CSRF tokens and will encrypt stored secrets.

## Configuration

| Variable | Default | Purpose |
|---|---|---|
| `FAULTBOX_SECRET` | **required** | At least 32 bytes. Keys for CSRF tokens and, later, for encrypting secrets are derived from it. |
| `FAULTBOX_PORT` | `8080` | HTTP port. |
| `FAULTBOX_DATABASE_PATH` | `faultbox.db` (`/data/faultbox.db` in Docker) | SQLite file. Created and migrated on startup. |
| `FAULTBOX_ADMIN_EMAIL` / `FAULTBOX_ADMIN_PASSWORD` | — | Create the first dashboard user, but only while no users exist. The password must be at least 12 characters. |
| `FAULTBOX_PUBLIC_URL` | taken from the request | External URL shown as the DSN in Laravel setup snippets. |
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

## HTTP endpoints

| Endpoint | Auth | Purpose |
|---|---|---|
| `GET /health` | none | Liveness: the process is serving HTTP. |
| `GET /ready` | none | Readiness: the database answers and the ingestion queue has room. |
| `POST /api/v1/events` | project API key | Report an exception or a message. Answers `202` right away; the event is stored a moment later. |
| `POST /api/v1/queue-failures` | project API key | Report a failed queue job. |
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

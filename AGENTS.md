# Agent guide

## Project map

- This directory is the Go server repository (`go.mod`, `cmd/`, `internal/`, `migrations/`, `web/`, `tests/`).
- The workspace parent contains the product docs in `../docs/`: `prd.md`, `roadmap.md`, `upcoming-features.md`, and `ui-redesign-v2.md`.
- The Laravel SDK is a sibling project in `../faultbox-laravel/`.
- Preserve uncommitted work. Check `git status` and inspect the existing diff before editing; do not commit unless asked.

## Product and runtime constraints

- Faultbox is self-hosted error and queue monitoring for Laravel. Keep the server as a small, single Go binary using SQLite.
- The default deployment budget is **128 MB RAM and 0.5 CPU**. The container sets `GOMEMLIMIT=96MiB`; treat memory and query count as product requirements.
- SQLite has one serialized writer and read-only reader connections. Keep writes short, batch where appropriate, add indexes for list filters, and avoid per-row queries.
- There is no Node.js runtime, frontend build, CDN, chart library, or third-party runtime dependency. HTML is rendered with Go templates and enhanced with HTMX and small vanilla JS/CSS.
- Migrations are embedded from `migrations/*.sql` and Goose applies pending migrations when the application opens the store during startup. Add schema changes as a new numbered SQL migration; never edit an already released migration. Startup must fail clearly if a migration fails.
- Keep the existing dark and light themes, strict CSP, accessibility, and data-scrubbing guarantees. Use `BRAND.md` for colors and voice.

## Coding conventions

- Follow existing Go package boundaries and standard formatting (`gofmt`). Keep implementation simple and use the standard library unless a dependency is clearly necessary.
- Validate input at boundaries, pass `context.Context` through database and request work, wrap errors with useful operation context, and use parameterized SQL.
- Keep dashboard behavior server-rendered first. Forms and filters should work through URLs/GET requests; JavaScript is progressive enhancement.
- Keep CSS tokens centralized in `web/static/css/app.css`. Do not add inline scripts/styles because the Content Security Policy blocks them.
- Avoid collecting or displaying sensitive values. Extend the existing scrubber and add coverage when new telemetry fields are introduced.

## Tests and verification

- **Every feature and bug fix needs a focused regression test.** Update the closest existing suite; add tests for success, empty/invalid input, and relevant failure paths.
- Place tests by scope: `tests/unit/` for isolated behavior, `tests/integration/` for real SQLite/store behavior, and `tests/feature/` for user-visible flows. Tests needing unexported helpers belong beside the implementation in an `internal/` `_test.go` file.
- Use `tests/testutil/` helpers and fixtures instead of sleeps. Ingestion is asynchronous; call `WaitProcessed()` before assertions.
- Useful commands: `go test ./...`, `go test -race ./...`, `go vet ./...`, and `golangci-lint run ./...`. For dashboard performance, use `FAULTBOX_SEED=200000 go test -run TestDashboardPerformance -timeout 30m ./tests/integration/`.
- Keep the documented 128 MB / 0.5 CPU deployment target and 300 ms p95 dashboard target in mind when changing data access.

## Current UI direction

- The reference screenshots are inspiration for layout and density, not Faultbox branding or evidence that every shown feature exists.
- Follow `../docs/ui-redesign-v2.md` for the planned project/environment scope selector, grouped sidebar, page-level filters, dark-first neutral palette, compact issue rows, metric cards, and shared tables.
- Requests and successful job runs require new telemetry and are tracked in M10; the UI redesign is M11. Do not present uncollected data as real: use explicit empty states until ingestion exists.

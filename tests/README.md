# Tests

| Folder | What goes here | Speed |
|---|---|---|
| `unit/` | One function or handler at a time, in memory. No network sockets, no files, no database. HTTP handlers are called with `httptest.NewRecorder`. | Milliseconds |
| `integration/` | Components against real dependencies, without HTTP. Today that means `store` against a real SQLite file in `t.TempDir()`: migrations, queries, sessions, concurrent writes. | Fast |
| `feature/` | Behaviour a user or the Laravel SDK would see. Tests run a full Faultbox instance (`testutil.NewApp`) on a local port with a temporary database, and drive it with a cookie-jar "browser" or API calls. | Slowest |
| `testutil/` | Shared helpers, not a test suite: `NewApp`, `Login`, `CreateProject`, `CSRF`, `DecodeJSON`, and so on. | — |

These suites can only call exported APIs. A test that needs unexported code goes in a `_test.go` file next to that code in `internal/`.

`testutil.Config` runs bcrypt at its minimum cost, so logging in during tests stays fast. Production uses cost 12.

## Running

```bash
go test ./...                     # everything
go test ./tests/unit/...          # unit only
go test ./tests/integration/...   # integration only
go test ./tests/feature/...       # feature only
go test -race ./...               # with the race detector (needs cgo; on by default in CI)
```

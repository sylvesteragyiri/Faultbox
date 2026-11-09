# Tests

| Folder | What goes here | Speed |
|---|---|---|
| `unit/` | One handler or function at a time, in memory. No network sockets, no files, no database. Uses `httptest.NewRecorder`. | Milliseconds |
| `feature/` | Behaviour a user or the Laravel SDK would see, over real HTTP against a server on a local port (`httptest.NewServer`). Once SQLite arrives, these tests use a temporary database. | Slower |
| `testutil/` | Shared helpers. Not a test suite. | — |

These suites can only call exported APIs. A test that needs unexported code, such as the internals of fingerprint normalisation, goes in a `_test.go` file next to that code in `internal/`.

## Running

```bash
go test ./...                 # everything
go test ./tests/unit/...      # unit only
go test ./tests/feature/...   # feature only
go test -race ./...           # with the race detector (needs cgo; on by default in CI)
```

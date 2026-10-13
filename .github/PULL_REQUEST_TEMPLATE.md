## Why

<!-- The problem this solves, with the issue it closes if there is one -->

## What changed

<!-- The behavior a user sees change, not every file -->

## Checks

- [ ] `gofmt -l .` prints nothing, and `go vet ./...` and `golangci-lint run ./...` pass
- [ ] `go test -race ./...` passes
- [ ] CHANGELOG.md has an entry under the next version

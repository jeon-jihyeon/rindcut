# Contributing to rindcut

Thanks for helping. Bug reports and fixes are welcome. For a security problem use the private reporting link in [SECURITY.md](SECURITY.md) instead of an issue.

## What you need

- Go at the version in [go.mod](../go.mod)
- [golangci-lint](https://golangci-lint.run) v2, the version CI pins in [test.yml](workflows/test.yml)

## Checks

Run these before opening a pull request. CI runs them on Linux, and runs the tests on macOS too.

```sh
gofmt -l .
go vet ./...
golangci-lint run ./...
go test -race ./...
```

- `gofmt -l .` prints nothing when every file is formatted

## Commits and pull requests

Commit subjects follow [Conventional Commits](https://www.conventionalcommits.org) as `type(scope): subject`, such as `fix(rindcut): report a missed run once`.

- `type` is one of `feat`, `fix`, `docs`, `test`, `refactor`, `chore` or `ci`
- Release notes group `feat` under Features and `fix` under Fixes, and leave merge commits out
- Keep one change per pull request and fill in the template

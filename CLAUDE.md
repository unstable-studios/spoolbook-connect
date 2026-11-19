# CLAUDE.md

spoolbook-connect: a Prusa Connect Local module (Go) that keeps spoolbook's
spools in step with Connect's Materials extension. Design:
`unstable-studios/spoolbook` `docs/spikes/prusa-connect-local.md`, issue
unstable-studios/spoolbook#239.

## Commands

Go isn't installed on the maintainer's host; use podman:

```bash
podman run --rm -v "$PWD":/src:Z -v gomod:/go/pkg/mod -w /src docker.io/library/golang:1.27 go test ./...
```

## Conventions

- Feature branches and PRs; never commit to `main`. Conventional commits
  (`feat(bridge): ...`), lowercase, imperative, no trailing period.
- `gofmt`, `go vet` and `go test` must pass (CI runs all three).
- US English in code, docs and UI text.

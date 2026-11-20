# CLAUDE.md

spoolbook-connect: a Prusa Connect Local module (Go) that keeps spoolbook's
spools in step with Connect's Materials extension. Design:
`unstable-studios/spoolbook` `docs/spikes/prusa-connect-local.md`, issue
unstable-studios/spoolbook#239.

## Commands

Go isn't installed on the maintainer's host; use podman:

```bash
podman run --rm -v "$PWD":/src:Z -v gomod:/go/pkg/mod -w /src docker.io/library/golang:1.27 go test ./...
sh scripts/release.sh            # release/ and the .tar.gz Connect installs (inside the container)
sh e2e/run.sh                    # end to end against Connect's demo; see the header for the podman line
```

## Layout

- `main.go`: SDK glue only (handshake, `plugin.configure`, the event stream,
  routes). Keep logic out of it.
- `internal/bridge`: events to spoolbook updates, links, the outbox. No SDK
  imports; tested with `MemKV` and fakes.
- `internal/connect`: the SDK adapters (record store, Materials'
  `routes.call`, the event dispatcher, this module's routes and page).
- `internal/spoolbook`: the token API client.
- `module.yaml`, `openapi.json`: checked by tests with the SDK's own parsers.
  `Settings` in `main.go` must match `options` (strict decoding).

## Conventions

- Feature branches and PRs; `main` is protected. Conventional commits
  (`feat(bridge): ...`), lowercase, imperative, no trailing period.
  release-please bumps `module.yaml` and `main.go` (`x-release-please-version`).
- `gofmt`, `go vet` and `go test` must pass (CI runs all three).
- Never log or return the token in an error.
- The SDK is `github.com/prusa3d/prusa-connect/sdk` at its `sdk/v*` tag.
  Materials' routes and events are its public contract (its README, "For
  other extensions"); read field names from its `openapi.json`, don't guess.
- US English in code, docs and UI text; UI text says what to do, briefly.

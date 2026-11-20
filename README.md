# spoolbook-connect

A [Prusa Connect Local](https://github.com/prusa3d/prusa-connect) module
that keeps [spoolbook](https://spoolbook.unstablestudios.com)'s spools in step
with Connect's Materials extension.

Materials already knows which spool is on which tool and how much it has
left, from the printer and the spool's NFC tag. This module passes that on to
your spoolbook library:

- **Amount left.** When a linked spool's `remaining_weight_g` changes in
  Materials, spoolbook's amount left for that spool follows. A spool counting
  down sends once it settles (2 minutes after its last change), not once per
  gram.
- **Prints.** When a print starts, the module notes each tool's spool and its
  amount left. When the print finishes or fails, each linked spool gets the
  print in its history: the file, date, tool and the grams it took (the drop
  in its amount left), sent together with the new amount so nothing comes off
  twice. A print that took nothing measurable, or whose spool was swapped
  mid-print, is recorded with 0 g. A cancelled print records no print; the
  spool's amount left is still sent.

Updates wait in the module's store until spoolbook takes them, so a restart
or a network outage loses nothing. Each print has a fixed id (SHA-256 of the
printer, job and tool), so a retry never counts a print twice.

Design: [spoolbook's spike](https://github.com/unstable-studios/spoolbook/blob/main/docs/spikes/prusa-connect-local.md),
issue unstable-studios/spoolbook#239.

## Requirements

- Prusa Connect Local 0.1.0-beta.1 or later, with a record backend declared
  under `resources.db` (the module keeps its links and queue there).
- The Materials extension (`github.com/prusa3d/materials`), 0.2.5 or later.
- A spoolbook library you can edit.

## Install

Connect installs a module from a release: a directory, or a `.tar.gz` of
one, holding `module.yaml`, a binary per platform and `checksums.txt`.
Download `spoolbook-connect_<version>.tar.gz` from this repository's
[releases](https://github.com/unstable-studios/spoolbook-connect/releases)
(linux/amd64, linux/arm64 and darwin/arm64), then on the Connect machine:

```sh
prusa-connect module add github.com/unstable-studios/spoolbook-connect@0.1.0 \
  -from spoolbook-connect_0.1.0.tar.gz -approve
```

The install prints what the module asks for. Read it before you run it with
`-approve`:

- `observe` (as the viewer role on the organization) to follow the printers'
  print events and the Materials events.
- Calls to Materials' routes (`GET /printers/tools`, `GET /spools`).
- Connections to the host in `spoolbook_url` only.

To install from the Extensions page instead, name the unpacked release under
`core.modules_local` (see Connect's "Writing a service module" docs).
Restart Connect if it was running.

Building it yourself: `sh scripts/release.sh` writes `release/` and the
`.tar.gz` (needs Go 1.26.11 or later).

## Set up

1. **Make a token in spoolbook.** Open Manage libraries, pick the library,
   then Tokens. Name it (for example "Prusa Connect") and copy it: it is
   shown once. The address shown there,
   `…/api/libraries/<library>/spools/…`, holds the library id.
2. **Set the module's options** under Settings, Extensions, spoolbook,
   Settings (or in the instance's `config:` block):

   | option | what |
   | --- | --- |
   | `token` | the token from step 1 (stored as a secret) |
   | `library` | the library id |
   | `spoolbook_url` | `https://spoolbook.unstablestudios.com` unless you run your own |

3. **Link spools.** Open
   `/api/v1/modules/com.github.aux.unstable-studios.spoolbook-connect/page`
   on your Connect. It lists Materials' spools; paste each one's spoolbook
   spool, either as `vendor/filament/spool` or as the spool page's address
   (`…/app/spool/<vendor>/<filament>/<spool>`), and press Link. The same is
   available as JSON:

   ```sh
   M=http://prusa-connect.local:8080/api/v1/modules/com.github.aux.unstable-studios.spoolbook-connect
   curl -H "Authorization: Bearer $PCL_TOKEN" $M/spools
   curl -H "Authorization: Bearer $PCL_TOKEN" -X PUT $M/links/12 \
     -d '{"vendor":"prusament","filament":"pla-galaxy-black","spool":"a1b2"}'
   curl -H "Authorization: Bearer $PCL_TOKEN" -X DELETE $M/links/12
   curl -H "Authorization: Bearer $PCL_TOKEN" $M/status
   ```

   `GET /status` shows the updates waiting to go, each with its last error,
   and the prints in progress.

## Delivery

- Retries with backoff (30 s doubling to 1 h) on 429 (honoring
  `Retry-After`), 5xx, 409 (a conflict, or a library with problems to fix in
  the app) and network errors.
- Gives up on 400, 401, 403, 404, 413 and 422, logging each once: the token
  is wrong or revoked, the spool doesn't exist, or the update doesn't fit.
- A spool's updates go in order: a newer amount left waits behind an older
  print that hasn't gone yet.
- Requests time out after 30 s, never follow a redirect, and the token never
  appears in a log line or an error.

## What's verified, and what isn't

Checked against the SDK and Materials source at Connect `v0.1.0-beta.1`, and
run end to end (`e2e/run.sh`) against Connect's own demo server with
Materials installed and a stand-in for spoolbook:

- The manifest parses with the SDK's own parser (`manifest.Parse`), and the
  route document with the host's (`routesapi.ParseFragment`); both are tests.
- `prusa-connect module add` installs it with `-approve` and `-settings`, the
  host starts it, sends the settings, serves its routes, and the module
  subscribes to `device.job.started`, `device.job.settled` and
  `materials.spool`.
- `job.*` events name the printer in the envelope's `subject`
  (`device:<id>`), as Materials itself reads it, and `routes.call` to
  Materials' `/printers/tools` and `/spools` works as the module itself and
  for the person on the page.
- Prints reach spoolbook with the token, file name, date, tool and print id;
  an amount left set in Materials reaches spoolbook after the delay.

Not verified yet:

- **Grams.** The demo's simulated printers report no filament use, so
  Materials' weights never drop there and every demo print went with 0 g.
  The drop in `remaining_weight_g` on a real printer (odometer or NFC tag)
  is the design, not yet seen.
- **The links page in a browser.** It's a plain HTML page with forms served
  from the module's own route. The host forwards `Content-Type`, but whether
  the Connect web app's session, CSRF rules or content security policy allow
  a form post to a module route hasn't been tried. The JSON routes work with
  a Connect token. A proper settings tab (`ui.contributes.settings`) needs a
  browser bundle built against `@prusa/connect-ui`, a follow-up.
- **Egress from an option.** `egress: option: spoolbook_url` is the declared
  form and parses; whether Connect enforces it at run time isn't known.
- **MMU slots.** A tool id like `0.2` is sent to spoolbook as tool 2.
- **Real spoolbook.** Tested against spoolbook's handler code and a stand-in,
  not yet against spoolbook.unstablestudios.com.
- **Marketplace.** Not published there; Connect's license terms (OCL with
  the NoSaaS addendum) are to be read before listing it.

## Development

```sh
podman run --rm -v "$PWD":/src:Z -v gomod:/go/pkg/mod -w /src docker.io/library/golang:1.27 go test ./...
```

- `main.go`: the SDK glue (handshake, settings, event stream, routes).
- `internal/bridge`: events to spoolbook updates, the links, the outbox.
- `internal/connect`: the record store, Materials' routes, the event
  dispatcher and this module's routes.
- `internal/spoolbook`: the HTTP client for spoolbook's token API.
- `e2e/`: the end-to-end run against Connect's demo.

The SDK is the Go module `github.com/prusa3d/prusa-connect/sdk`, fetched by
its `sdk/v0.1.0-beta.1` tag; no checkout or `replace` is needed.

## License

MIT

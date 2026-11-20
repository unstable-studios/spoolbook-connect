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
- **A print whose settle never arrives** (a gap in the event stream, or the
  module down past Connect's event buffer) can't hold its spool's updates
  back. The module checks Connect's own record of the printer
  (`device.jobs`) whenever that spool reports, and every 5 minutes: if
  Connect's last print is this one and it ended, the print is recorded from
  that record; if the printer has another print or none, it is forgotten. It
  is also forgotten when the printer starts another print or its spool
  leaves the tool. If Connect can't be asked, the bound is 48 hours.

Updates wait in the module's store until spoolbook takes them, so a restart
or a network outage doesn't lose them. Some are dropped on purpose (see
Delivery). Each print has a fixed id (SHA-256 of the printer, job, the job's
start time and tool), so a retry or a replayed event never counts a print
twice, and a job number a printer reuses after a reset is a new print.

**Grams and timing.** A settled print is handed to a worker, so the event
stream never waits on it, and it is kept in the store until its updates are
queued (a restart picks it up again). The worker reads the tools every 10 s
until the weights have settled, because Materials may still be counting the
last of the print. A read counts as settled at once when it matches the
amount Materials' own spool event reported after the print ended. Otherwise
the weight has to move since the first read and then read the same twice,
or 2 minutes have to pass. Materials reports once per whole gram, so a print
that took under a gram waits the 2 minutes. If Materials doesn't answer, it
retries with backoff for up to 24 hours, then drops the print and logs it.
Once a print is settled, each linked spool is read once more, and an amount
left that was held back while it settled goes out.

A settle stuck in that backoff reads the weights only once Materials
answers. If the printer has started another print by then, the grams can
include some of that print's filament. The amount left is still right. What is left of
that race: filament Materials counts more than 2 minutes after the print
settled, or a purge or unload in that window, lands in the next amount left
rather than in the print's grams. Either way spoolbook's amount left ends up
right; only how the grams are split between the print and the spool's other
use can be off.

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
  print events, read each printer's current print, and the Materials events.
  An open print counts as Connect's print only when both its job id and its
  start time match, so a job id used again can't be taken for an older
  print.
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
   A="Authorization: Bearer $PCL_TOKEN"
   curl -H "$A" $M/spools
   T=$(curl -s -H "$A" $M/token | sed 's/.*"token":"\([0-9a-f]*\)".*/\1/')
   curl -H "$A" -H 'Content-Type: application/json' -X PUT "$M/links/12?token=$T" \
     -d '{"vendor":"prusament","filament":"pla-galaxy-black","spool":"a1b2"}'
   curl -H "$A" -X DELETE "$M/links/12?token=$T"
   curl -H "Authorization: Bearer $PCL_TOKEN" $M/status
   ```

   `GET /status` shows the updates waiting to go, each with its last error,
   and the prints in progress.

## Delivery

- Retries with backoff (30 s doubling to 1 h) on 5xx, 409 (a conflict, or a
  library with problems to fix in the app) and network errors.
- On 429, nothing goes until the `Retry-After` (capped at 1 h): the limit is
  the token's, not the spool's.
- On 401 (the token was revoked or rotated, or its maker is no longer an
  editor), 403 (a library limit or role) or a redirect (`spoolbook_url`
  moved), the whole queue pauses: 15 minutes, doubling to 6 hours. Nothing
  is dropped. `GET /status` and the links page show why while it holds. It
  resumes when a send works, or when the module starts with different
  settings (url, library or token); a restart with the same settings keeps
  the pause and its wait.
- On 404 (no such spool, perhaps linked before it was made in spoolbook),
  that spool's updates are kept and tried every 1 to 6 hours. After 7 days
  they are dropped and logged.
- Gives up on 400, 413 and 422, logging each once: the update doesn't fit.
- A queued record that can't be read is moved aside to `bad/` in the store
  and logged; the rest still goes.
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
  from the module's own route. Whether Connect's web session reaches a module
  route from a browser (its host auth) hasn't been tried. The JSON routes
  work with a Connect token. Each form carries a token bound to the person
  Connect says is asking (an HMAC with a key kept in the store), because
  Connect passes a module no cookie, `Sec-Fetch-Site` or custom header to
  check (`routesapi` forwards an allow-list). `PUT` and `DELETE` on `/links`
  take the same token as `?token=` (from `GET /token`), and `PUT` takes only
  `Content-Type: application/json`. A token carries its key's epoch. The key
  is replaced every 30 days, and the one before it is still accepted for a
  day after. A request Connect hands over with no signed-in person is
  refused ("Sign in to Connect"), rather than sharing one token. The page and
  link routes send `Cache-Control: no-store` and `Referrer-Policy:
  no-referrer`. Connect only forwards `Cache-Control`. The
  page sends `X-Frame-Options: DENY` and a strict CSP, but Connect's docs say
  the host keeps its own security headers and drops a module's, so those may
  never reach the browser. A proper settings tab (`ui.contributes.settings`)
  needs a browser bundle built against `@prusa/connect-ui`, a follow-up.
- **Egress from an option.** `egress: option: spoolbook_url` is the declared
  form and parses; whether Connect enforces it at run time isn't known.
- **Tool numbers.** As PrusaSlicer numbers filaments: `e` or `e.0` is tool
  e, `0.s` (an MMU on the one extruder) is tool s. Anything else (an MMU on
  a later tool) is sent without a tool number, logged once.
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

OCL v1.1 + Micro v1 + NoSaaS v1 + SWAtt v1, the same terms as Prusa Connect,
which this module is built on. In short (the full texts in [LICENSE](LICENSE)
are what count):

- Non-commercial use: use, change and share it. Anything you distribute
  that's derived from it stays under these terms and keeps the
  "built on" attribution.
- A business may use it internally, but may not copy, sell or offer it (or
  its API) as a service to others without a separate license.
- A business with more than €1M in yearly revenue needs a separate license
  even for internal use.

For a license, contact Unstable Studios; parts that come from Prusa Connect
are Prusa Research's to license. Versions 0.1.1 and earlier were released
under the MIT License.

Built on Prusa Connect by Prusa Research.

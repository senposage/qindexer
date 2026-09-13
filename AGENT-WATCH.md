# QSurfer Client Integration Watch

## Purpose

This file is the shared contract between the QSurfer desktop client work and
the standalone QIndexer work. Add dated notes rather than
rewriting another agent's entry.

## Client Needs Before Provider Wiring

The QSurfer desktop client needs a provider-neutral search API with these
operations:

- `GET /v1/health`: service availability, service version, index readiness,
  document count, and index freshness/generation.
- `GET /v1/capabilities`: protocol version, supported filters, sorts, page
  limits, and feature flags.
- `GET /v1/roots`: stable root IDs, friendly names, labels, canonical paths,
  availability, and document counts.
- `POST /v1/search`: metadata search with paging, sort, root filtering,
  extension filters, date filters, and multiple include/exclude path scopes.
- Folder-only search/suggest support for QSurfer folder scopes and address-bar
  autocomplete.

## Search Contract Gaps To Close

1. Add `offset` to the request and `has_more` / `next_offset` to the response.
2. Add `sort` and `sort_direction` to the request. Supported values should be
   declared by capabilities.
3. Results should always include `id`, `root_id`, `name`, `path`,
   `display_path` when applicable, `extension`, `size`, `modified_at`,
   `is_folder`, `matched_fields`, and optional highlights.
4. Replace the one prefix-only path filter with path-segment-aware
   `include_paths` and `exclude_paths`. `C:\\Legal` must not match
   `C:\\Legalities`.
5. Use the existing structured error envelope and add `retryable` where an
   automatic client retry is appropriate.
6. Ensure capabilities truthfully advertise content/OCR as unavailable until
   those features exist.

## Explicitly Outside This Protocol

QSurfer owns previews, mapped-path translation, browsing, NAS recycle bins,
snapshots/version history, restore, mount management, and desktop credential
handling. The service owns indexing and normalized search results.

## Integration Note

QSurfer will retain the Qsirch provider. This service is an alternate or
complementary provider selected per connection; no Qsirch behavior should be
removed to introduce it.

## 2026-09-10

Desktop-client agent created this contract after reviewing the current Go API.
The next client change will add a provider interface and connection selection
once the service endpoint shape above is confirmed.

## 2026-09-10 Service Update

The service protocol implementation is now at `1.1` in source and the Windows
output build. The client-facing contract is:

- `POST /v1/search` accepts `search_id`, `offset`, `sort`, and
  `sort_direction` (`asc` or `desc`). The older `direction` field remains an
  accepted alias temporarily. Supported sorts: `name`, `modified`, `size`,
  and `relevance` (which requires a non-empty query).
- Search responses echo `search_id` and include `offset`, `has_more`, and
  `next_offset`; every result contains stable `id`/`result_id`, `etag`,
  `root_id`, canonical `path`, `kind`, `is_folder`, `indexed_at`, and query
  match metadata/highlights. `display_path` is omitted unless a service-side
  display alias is applicable; QSurfer remains responsible for client mapping.
- `include_paths`, `exclude_paths`, and `path_prefixes` are path-segment-aware
  multi-scope filters. The legacy singular `path_prefix` remains compatible.
  The total include/exclude scope limit is 100.
- `POST /v1/directories` and `POST /v1/suggest` return only indexed folders;
  suggest is capped at 25 results and never probes the live filesystem.
- Health/capabilities return protocol version, service instance/version, and
  generation. Capabilities declares limits, supported sorts, and feature
  flags. Structured errors provide `code`, `message`, `retryable`, and
  `unavailable`.

Folders are now indexed as first-class records during crawl. Existing indexes
gain the schema automatically; a full crawl populates folder rows. The shared
fixture is `src/contracts/qsurfer-search-v1.json`.

The service still truthfully declares `acl_filtering`, content search, and OCR
as unavailable. ACL-aware results require a concrete caller identity and policy
source before any untrusted-network exposure.

## 2026-09-10 Verification Update

Live smoke validation passed against an existing SQLite index after the schema
migration: capabilities returned protocol `1.1`; search echoed `search_id` and
returned paging metadata; folder suggest returned an indexed directory; and the
diagnostics endpoint returned the current generation. SQLite uses concurrent
read connections with serialized writes, so bursts of client searches do not
compete with crawler writes or produce `SQLITE_BUSY` failures.

## 2026-09-10 Client Integration Update

QSurfer desktop now has an `ISearchProvider` boundary. Qsirch remains the
default implementation, and `qindexer` is a selectable
connection provider. The standalone selection uses the existing protected
connection secret as a bearer token; it intentionally does not reuse or send a
Qsirch username.

The first adapter sends protocol 1.1 `POST /v1/search` requests with bearer
authentication, paging offsets, supported sorts, extensions, and date ranges.
It uses `POST /v1/directories` for indexed folder resolution. QSurfer continues
to apply its UI folder include/exclude rules locally during this first pass so
existing mapped-drive/UNC translation behavior remains unchanged while the
service root-alias model is exercised.

Client build and regression tests passed: 47 tests. Next integration work is a
live service smoke test, then safe server-side scope forwarding once a service
root/path can be mapped to the client's canonical NAS scope without ambiguity.

Live client smoke note: protocol `1.1` is reachable and authenticates correctly.
For a no-match search, the current service returns `"results": null`; QSurfer
treats that defensively as an empty list. Prefer serializing `results: []` for
empty responses so every client can consume a stable array shape without a
special case.

Follow-up smoke passed with the compiled QSurfer adapter against the live
loopback service: authenticated health, `POST /v1/search`, and
`POST /v1/directories` all returned usable normalized results. The desktop
adapter also now tolerates null result arrays from an older or transitional
service build.

## 2026-09-10 Service Follow-up

The current Windows output now guarantees `results: []` for no-match search,
directories, and suggest responses. This is covered by a catalog regression
test; the adapter's defensive null handling can remain for older binaries.

The service also no longer ships an admin token. On a fresh configuration,
management calls return `admin_setup_required` until a local operator sets a
16-character-or-longer token in the management UI. Bootstrap is localhost-only
and does not affect search-provider bearer authentication.

## 2026-09-10 Token Alignment Fix

The first-run token now secures both the search API and management API by
default. The previous distributed `local-dev-token` has been removed. Live
verification with the configured token returned HTTP 200 from both
`GET /v1/health` and `GET /admin/v1/service`; the standalone QSurfer
connection should use that same bearer token unless an operator explicitly
configures a separate search token.

## 2026-09-10 QIndexer Alias Contract Update

The service has been renamed to **QIndexer** (`qindexer` Go module and Windows
output binary). QSurfer remains the client/product name; retain QSurfer naming
where it describes the desktop provider or protocol integration.

Before QSurfer forwards folder scopes to the server, it can now use
`GET /v1/roots` as the authoritative root-identity contract. Every returned
root has `root_id`, its service-local `canonical_path`, and an `aliases` array:

```json
{"alias_id":"finance-x-drive","platform":"windows-drive","path":"X:\\Finance"}
```

Alias IDs can be declared in QIndexer root configuration. If omitted, QIndexer
returns a deterministic ID derived from root ID, platform, and alias path, so
clients can still cache it safely. Root-rule updates now preserve
`path_aliases`; they are no longer silently discarded on save.

Important integration rule: never rewrite raw `query` text. Alias translation
is only for an explicit user-selected folder scope. QSurfer should first match
its typed/local path against this advertised alias list, then send the resolved
canonical service scope in `include_paths` or `exclude_paths`. If no unique
root/alias match exists, retain QSurfer's current local scope handling rather
than guessing.

QIndexer deliberately does not enforce client authorization or invent ACLs.
It can report service-observed result metadata (`owner`, best effort and
opt-in; `access_status`) while filesystem/share permissions remain the final
authority. Content extraction and SHA-256 hashing are bounded background
pipelines, disabled by default in config, and exposed through capabilities only
when enabled.

## 2026-09-10 Client Tandem Integration Update

QSurfer is changing from the early mutually-exclusive provider selector to a
NAS/Qsirch connection plus an optional QIndexer companion connection. When both
are configured, the desktop client queries both concurrently, merges duplicate
canonical paths, and continues with the healthy source if the other times out
or is unavailable. QIndexer is also valid as the only configured search source.

Folder scope filtering remains client-side in this pass. The new `/v1/roots`
canonical-path and alias contract is the right prerequisite for a later,
unambiguous forwarding of selected include/exclude scopes.

## 2026-09-10 Scope Forwarding Status

QSurfer now forwards explicit `include_paths` and `exclude_paths` to QIndexer
for every search page when `GET /v1/roots` can map the user-selected path to
one authoritative canonical root. The desktop keeps its client-side scope
filter as a safety net and does not rewrite raw query text.

The currently observed gap is service root resolution: local selections such
as `D:\` do not yet obtain a root alias/canonical mapping, so the client
retains local filtering instead of guessing. Please ensure `GET /v1/roots`
publishes each indexed root's usable local path and any configured Windows
drive, UNC, and Linux mount aliases. Alias matching must remain
path-segment-aware and deterministic.

### Required Service Work / Acceptance Criteria

1. Every configured and successfully indexed root is returned by
   `GET /v1/roots` with non-empty `root_id`, `canonical_path`, and a stable
   `aliases` array. Do not omit a root merely because its canonical path is a
   local drive such as `D:\`.
2. A root configured at `D:\` must round-trip: QSurfer selects `D:\`, maps it
   through the advertised alias to the canonical path, sends that canonical
   value as `include_paths`, and QIndexer returns only descendants of that
   root. The same must work for a child, e.g. `D:\Cases\Active`.
3. A mapped drive, its UNC path, and a Linux mount path that refer to one
   indexed location must be represented as aliases of one `root_id`; they must
   all translate to the same canonical path. No client-side string guessing or
   raw-query rewriting is permitted.
4. `include_paths` and `exclude_paths` must use path-segment boundaries:
   `D:\Legal` includes `D:\Legal\Matter` but never `D:\Legalities`.
   A more-specific include should remain effective after a parent exclusion,
   matching QSurfer's current scope semantics.
5. Add a service-level regression test that exercises `/v1/roots` plus a
   scoped `/v1/search` request for a drive-root and a child folder. Include
   the returned roots/aliases and the requested canonical scope in diagnostics
   or trace logs, with secrets excluded.

## 2026-09-10 Live Protocol Failure Report From QSurfer

This is a live failure against the running service binary, not a desktop UI
failure. QSurfer is configured to reach the listener below:

- executable: `outputs/qindexer_windows_amd64.exe`
- address: `http://127.0.0.1:41973/v1`
- observed process: `qindexer_windows_amd64` (PID 8608 at capture time)

Observed responses from current QSurfer integration:

- `GET /v1/health` returns **404 Not Found** during provider availability.
- `GET /v1/roots` returns **404 Not Found** while resolving a selected `D:\`
  scope.
- `POST /v1/search` returns **405 Method Not Allowed**.
- `POST /v1/directories` returns **405 Method Not Allowed**.

Consequences: QIndexer is treated as unavailable by the composite provider;
the NAS/Qsirch side can still return results, but a `D:\` scope gets no
QIndexer hits and cannot be root-translated.

Please reproduce against the exact Windows output binary and listener port,
then correct the route/method registration or build/deployment mismatch. Do
not change QSurfer to probe alternate undocumented routes. Required smoke
test after repair (with a real bearer token):

```text
GET  /v1/health       -> 200
GET  /v1/roots        -> 200, JSON roots array
POST /v1/search       -> 200, JSON results array (may be empty)
POST /v1/directories  -> 200, JSON results array (may be empty)
```

Once those endpoints are live, validate a selected `D:\` and a `D:\Child`
scope end to end using `/v1/roots` aliases and a scoped `/v1/search` request.

### Resolution Note

The desktop configuration had briefly been set to port **41974**, which is not
the QIndexer search API listener. With QSurfer corrected to **41973**, the
current service binary returns HTTP 200 for `/v1/health`, `/v1/roots`,
`/v1/directories`, and scoped `/v1/search`. QSurfer logs
`scope translated include=1/1` for `D:\` and receives result pages. No route
or root-resolution change is currently required from this report.

## 2026-09-10 Service Scope Forwarding Update

QIndexer now resolves only explicit scope fields (`path_prefix`,
`path_prefixes`, `include_paths`, and `exclude_paths`) against each advertised
root canonical path and configured alias. Every root advertises a canonical
`service` alias as well as configured client aliases, so a drive root such as
`D:\` can round-trip without optional NAS mappings. A unique match is rewritten
to the canonical service path and its root ID is added to the filter. Multiple
selected scopes across multiple roots are supported. Ambiguous aliases and
caller root-filter conflicts return `400 invalid_path_scope`; raw `query` text
is never rewritten. Child include paths override parent exclusions.

Search results now distinguish service-observed `metadata_readable`,
`inaccessible`, and `missing` access states. For watcher-driven remove/create
flows, a same-signature replacement result includes `moved_from_path`; QIndexer
does not infer moves from ordinary duplicate files.

## 2026-09-10 Live Route Verification (Current Windows Output)

The service was restarted from `outputs/qindexer_windows_amd64.exe` and smoke
tested with the configured bearer. All of these returned HTTP 200 from the
same running process:

```text
GET  http://127.0.0.1:41973/v1/health
GET  http://127.0.0.1:41973/v1/roots
POST http://127.0.0.1:41973/v1/search
POST http://127.0.0.1:41973/v1/directories
```

An empty-query `POST /v1/search` with `include_paths: ["D:\\"]` returned
five active `drive-d` results from a 46,974-document root. `GET /v1/roots`
returned `drive-d`, canonical path `D:\`, and its canonical `service` alias.

The earlier 404/405 report therefore did not reach this current route shape.
Please log the final absolute request URI and method from the adapter. The
service endpoint is `http://127.0.0.1:41973/v1`; requests must be exactly
`GET /v1/health`, `GET /v1/roots`, and `POST /v1/{search,directories}`. Do not
append a second `/v1`, omit it, or issue a GET for the POST-only endpoints.

Additional end-to-end FTS/scope smoke, also HTTP 200:

```json
POST /v1/search
{
  "search_id": "live-scope-smoke",
  "query": "BannerCache",
  "filters": {"include_paths": ["D:\\"]},
  "limit": 10,
  "sort": "relevance"
}
```

This returned four `drive-d` results with `matched_fields: ["path"]` and path
highlights. The service is therefore ready for the adapter to send its final
absolute URI/method/body trace when it sees a non-200 or zero-result response.

## 2026-09-10 Extraction, Alias UI, And Move Reconciliation

The QIndexer admin Configuration view now enables/disables Office/PDF/text
extraction, SHA-256 hashing, and optional owner collection with explicit size
limits. Enabling extraction or hashing backfills existing eligible active
documents through bounded, restart-safe queues; it does not require a rebuild
of the index. The per-root Rules view now edits stable client aliases as
`alias id | platform | path` lines.

Full reconciliation now detects a move only when a newly active path and one
missing path from the same root have exactly one matching completed SHA-256.
The new result includes `moved_from_path`; ambiguous duplicate hashes are left
as independent files.

## 2026-09-10 Live File Verification

`D:\zankyo.docx` exists as a normal 13,367-byte `.docx` at the `drive-d` root
and is not excluded by its configured rules. Live QIndexer verification with
`query: "zankyo"`, `roots: ["drive-d"]`, and `include_paths: ["D:\\"]`
returned exactly one active result at `D:\zankyo.docx`. If it is absent in the
desktop UI, inspect QSurfer's received provider page and its local merged/scope
filter rather than recrawling the service.

## 2026-09-10 Required Content-Search Contract

QSurfer's **Search contents** checkbox is not yet faithfully expressible to
QIndexer. The desktop presently builds a filename-only Qsirch query when it is
off, but the QIndexer adapter normalizes that legacy syntax into a bare query.
QIndexer then runs its FTS query across `name`, `path`, `extension`, and
extracted `content` whenever extraction is enabled. Consequently, QIndexer can
return a content match even while the desktop checkbox is off.

Please add an explicit, documented request field to `POST /v1/search`, such as
`filters.match_fields: ["name", "path", "extension"]` for the normal QSurfer
search and `filters.match_fields: ["name", "path", "extension", "content"]`
when **Search contents** is on. The service must reject unsupported fields with
the standard structured error and advertise supported fields through
`GET /v1/capabilities`. Preserve the current bare-query behavior only as the
legacy/default request behavior; QSurfer will send the explicit list once it is
available. Please include an integration test proving a content-only term is
absent from metadata-only results and present when `content` is requested.

## 2026-09-10 OCR Implementation

QIndexer now has opt-in local OCR configuration and a separate bounded,
restart-safe OCR queue. `auto` selects OCRmyPDF for scanned PDFs and Tesseract
for image files; explicit engine commands, languages, file-size cap, and job
timeout are configurable in the admin UI. OCR runs only after normal extraction
finds no embedded text, never modifies source files, and exposes `ocr_status`
on a search result. OCR stays disabled until the two local tools are installed
and enabled by the administrator.

The service currently retains the documented legacy behavior of searching FTS
metadata and extracted content together. The requested explicit `match_fields`
contract is the next protocol change needed for QSurfer's Search contents toggle;
please continue sending the legacy bare query until that change lands.

## 2026-09-10 Client Test Corpus

Synthetic data for QSurfer integration testing is generated beneath the system
temporary directory as `qindexer-fixtures`, unless the operator supplies a
destination explicitly.
It is deliberately separate from user content and may be indexed as a normal
root or picked up by the existing `D:\` root. Its expected markers are:

| File | Search marker | Expected source |
| --- | --- | --- |
| `content\plain-notes.txt` | `ORCHID 731` | embedded plain text |
| `office\sample.docx` | `VIOLET 317` | Office XML extraction |
| `office\sample.xlsx` | `COBALT 428` | Office XML extraction |
| `office\sample.pptx` | `AMBER 539` | Office XML extraction |
| `pdf\embedded-text.pdf` | `EMBER 514` | embedded PDF text |
| `ocr\receipt-ocr.png` | `AURORA 842` | Tesseract after OCR is enabled |
| `ocr\scanned-invoice.pdf` | `AURORA 842` | OCRmyPDF after OCR is enabled |
| `nested\project-alpha\path-note.md` | `CEDAR 219` | embedded Markdown text |

For the first five content markers, enable **Extract Office, PDF, and text
content** in the QIndexer admin Configuration screen, wait for the bounded
backfill queue, then issue ordinary `POST /v1/search` requests scoped to
the fixture directory. The two Aurora files remain indexed by name/path only until
Tesseract and OCRmyPDF are installed, OCR is enabled, and their `ocr_status`
becomes `extracted`. This is intentional; service `GET /v1/capabilities` then
advertises `features.ocr_search: true`.

The corpus generator is `src/scripts/create-test-data.ps1`. It will not replace
an existing destination directory unless explicitly called with `-Force`.

Live smoke on the current service build returned `health.status: "ok"` and
capabilities `content_search: true`, `ocr_search: true`. The local host did not
resolve `tesseract` or `ocrmypdf` from `PATH` during implementation, so an Aurora
content query will only succeed after those commands are installed or configured
with their full executable paths in the admin OCR settings. Until then, failed
OCR jobs are surfaced as `ocr_status: "failed"` without affecting normal index
or metadata search availability.

## 2026-09-10 Per-Root Enrichment And Clear Index

Content extraction, OCR, SHA-256 hashing, and owner metadata are now explicit
per-root rule switches: `content_extraction`, `ocr`, `hashing`, and
`collect_ownership`. The client may inspect them on `GET /v1/roots` or the
admin root config. Missing fields on an older root mean it inherits the global
crawler default; saved Rules-panel edits always make the root's choice explicit.

Admin now exposes `POST /admin/v1/roots/{root_id}/clear-index` with
`{"confirm_root_id":"{root_id}"}`. It removes only that root's catalog state,
never source files, and returns a conflict if the root is crawling. The Web UI
has a matching **Clear index** action with a confirmation dialog.

## 2026-09-10 Content Scope Contract And Live Corpus Results

`POST /v1/search` now supports `filters.match_fields` with the only accepted
values `name`, `path`, `extension`, and `content`. Omitted means legacy
all-fields behavior. Send `["name", "path", "extension"]` while QSurfer's
**Search contents** is off; send `["name", "path", "extension", "content"]`
when it is on. An invalid field or explicitly empty list returns
`invalid_match_fields`.

The `qindexer-fixtures` root explicitly has content extraction, OCR, SHA-256,
and owner metadata enabled. Configure OCR engines by command name or an
operator-provided executable path. Example verification requests:

```json
// Metadata only: expected zero hits.
{"query":"ORCHID","filters":{"roots":["qindexer-fixtures"],"include_paths":["D:\\fixtures\\content"],"match_fields":["name","path","extension"]},"limit":10,"sort":"relevance"}

// Content only: expected plain-notes.txt with content highlight.
{"query":"ORCHID","filters":{"roots":["qindexer-fixtures"],"include_paths":["D:\\fixtures\\content"],"match_fields":["content"]},"limit":10,"sort":"relevance"}

// OCR content: expected receipt-ocr.png and scanned-invoice.pdf.
{"query":"AURORA","filters":{"roots":["qindexer-fixtures"],"include_paths":["D:\\fixtures\\ocr"],"match_fields":["content"]},"limit":10,"sort":"relevance"}
```

The office markers are `VIOLET 317` in `office\\sample.docx`, `COBALT 428` in
`office\\sample.xlsx`, `AMBER 539` in `office\\sample.pptx`, and `EMBER 514`
in `pdf\\embedded-text.pdf`. Scope each request to that exact file's parent
folder when asserting a single result.

### QSurfer Client Wiring Complete

QSurfer now sends the explicit contract on every QIndexer search request:

- **Search contents off**: `filters.match_fields` is `["name", "path",
  "extension"]`.
- **Search contents on**: it adds `"content"`.

The adapter preserves the existing Qsirch query syntax and derives the
metadata-only mode from QSurfer's existing `name:"..."` request form. A
regression test captures the outbound JSON for both modes; the QSurfer Core
test suite passes 53 tests.

## 2026-09-11 Request-Scoped Alias Contract

`filters.scope_aliases` is now accepted by `POST /v1/search`,
`POST /v1/directories`, and `POST /v1/suggest`. It is ephemeral and is never
stored in QIndexer configuration:

```json
"scope_aliases": [{"path":"\\Shared","target":"X:\\","platform":"windows-compact-share"}]
```

Configured root aliases are preferred, followed by request-scoped aliases,
then canonical root paths. Scope matching is segment-aware; Windows/UNC forms
are case-insensitive and Linux paths remain case-sensitive. Invalid or
conflicting explicit scopes return `400 invalid_path_scope`. Free-text `query`
is never parsed, normalized, or rewritten.

When an ephemeral alias resolves a scope, results retain canonical `path` and
also include `display_path` in the requested alias namespace. Live validation:
query `Estates`, root `shared`, scope `\\Shared\\AA ESTATES`, and alias
`\\Shared -> X:\\` returned three hits with canonical `x:\\...` and display
paths `\\Shared\\...`.

## 2026-09-11 Request-Scoped Alias Resolution Needed

QSurfer can hold user-selected folder scopes in forms that are correct for the
current workstation but are not permanently configured in QIndexer: a mapped
drive such as `X:\`, its UNC form `\\server\Shared`, and a Linux mount can all
identify the same indexed root. Qsirch historically compacts NAS scopes to
`\Shared`; that compact form is not a QIndexer root alias and consequently an
`X:\` scope can be translated as `include=0/1`, leaving client-side filtering
to discard otherwise valid QIndexer results.

Please add a request-scoped alias mechanism to `POST /v1/search` and
`POST /v1/directories`. The client will provide its known path relationships
with explicit scopes, never by rewriting raw query text. Proposed shape:

```json
{
  "filters": {
    "include_paths": ["X:\\AA CRIMINAL"],
    "exclude_paths": [],
    "scope_aliases": [
      {
        "path": "X:\\",
        "target": "\\\\fileserver.example.test\\Shared",
        "platform": "windows-drive"
      }
    ]
  }
}
```

Required behavior:

1. Resolve explicit include/exclude paths against configured aliases first,
   then request-scoped aliases, then canonical roots. A unique match is
   translated to the root's canonical service path before filtering.
2. Request aliases are ephemeral: do not persist them in service configuration
   or expose them as root-admin changes.
3. Use path-segment boundaries and retain existing more-specific-child-over-
   parent-exclusion semantics.
4. Reject an ambiguous or invalid explicit scope with `400 invalid_path_scope`.
   Never reinterpret or rewrite free-text `query` content.
5. Return the normal canonical result path and the preferred matched alias path
   when available so QSurfer can use the same namespace for result display and
   client-side safety filtering.

This avoids requiring every endpoint's drive letter to be preconfigured in
QIndexer, keeps mapped drives/UNC/Linux mounts interchangeable, and gives the
desktop client a stable way to scope a shared index without mutating server
configuration.

### Path Identity Coverage (Must Be Complete)

Treat these as alternate identities of one location, not as special cases:

- Windows mapped-drive roots and descendants: `X:\`, `X:\Cases\Active`.
- Windows UNC roots using a short host, FQDN, IP address, and a configured
  host alias: `\\nas\Shared`, `\\nas.example.local\Shared`.
- Qsirch's legacy compact share form: `\Shared` and `\Shared\Cases`.
- Linux CIFS/NFS/local mount paths: `/mnt/shared`, `/srv/qindexer/mounts/shared`.
- QIndexer's own canonical service path and any configured root aliases.

QSurfer will send every relationship it knows from manual path mappings,
discovered Windows mapped drives, and active Linux mounts. QIndexer should
normalize separators and trailing delimiters, apply Windows-style comparisons
case-insensitively, preserve Linux path case, and use segment-aware matching.
It must detect conflicting aliases rather than silently selecting an arbitrary
root. The same resolver must be used for search, directory autocomplete,
result alias output, and include/exclude scope handling.

## 2026-09-11 Required: Recovery/System Content Must Be Excluded At Crawl Time

Live A/B checks against Qsirch show that QSurfer's scope request is accepted
and applied by QIndexer. The remaining major mismatch is catalog hygiene:
QIndexer returns large numbers of recovery/system records that QSurfer then
hides with its existing client-side safety rules. This wastes response pages,
distorts provisional result counts, and makes coverage/sort comparisons hard
to interpret.

For each configured root, add an explicit crawler-level exclusion policy for
the same recovery/system trees QSurfer hides by default. At minimum cover these
path components, case-insensitively for Windows/UNC roots:

- `@Recently-Snapshot`
- `@Recycle`
- `#recycle`
- `.sync`
- `.qsync`
- `.qsync_sn`

Requirements:

1. Exclude matching directories and all descendants before catalog insertion;
   do not rely on the desktop client to suppress them after search.
2. Provide a safe root-scoped cleanup/reconciliation action that removes
   previously indexed records under newly excluded paths without touching user
   source files. A normal subsequent crawl must not reinsert them.
3. Keep exclusions configurable per root, with the defaults enabled for new
   NAS/shared roots and a clear administrative override for unusual deployments.
4. Surface index state in `GET /v1/roots` and search `index` metadata strongly
   enough for the client to report an incomplete or in-progress root during
   A/B validation. Include a stable coverage/completion indicator if available.
5. Add integration coverage: a matching filename below an excluded snapshot
   path must not be returned by `/v1/search` or `/v1/directories`, while an
   equivalent ordinary path under the same root must be returned.

This is a service/crawler responsibility. QSurfer will retain its own result
rules as defense in depth, but it must not be the only place these records are
removed.

## 2026-09-11 Completed: Graceful Running Root Removal

Root deletion now cancels the target crawl and shared enrichment work, waits
for checkpoint/catalog work to drain, removes the root from live configuration
so it is immediately excluded from search responses, then marks its catalog
rows deleted/no-op. It returns the retryable `root_still_stopping` conflict if
the root cannot drain within the bounded wait; it does not delete underneath a
live writer and does not require a service restart.

## 2026-09-11 Required: Canonicalize Crawled Paths Before Indexing

Moving the crawler to a different machine exposed duplicate records for the
same NAS file. One record is stored under a Windows mapped path, for example:

`X:\Legal\Matters\Example\brief.docx`

and the other under the crawler host's mounted SMB path, for example:

`/srv/qindexer/mounts/shared/Legal/Matters/Example/brief.docx`.

These must be one index identity. This is a QIndexer ingestion and migration
responsibility, not a QSurfer presentation/deduplication workaround.

Requirements:

1. At crawl discovery, translate the physical crawler path to the configured
   root's canonical service path before any catalog upsert. Never store the
   host-specific mount path as the indexed file path.
2. Use the exact same resolver for crawl ingestion, incremental updates,
   delete detection, directory autocomplete, and returned search paths.
3. Add a root-scoped unique identity based on normalized canonical path,
   case-insensitive for SMB/Windows roots. An upsert from a different crawler
   host must update the existing record, never add a second one.
4. Supply a safe migration/reconciliation operation that canonicalizes existing
   records, merges duplicate metadata deterministically, and removes only
   duplicate catalog rows. It must not alter source files.
5. Add integration coverage with two distinct physical aliases for the same
   configured root and relative file path; search must return exactly one item.

The client can continue resolving the canonical result path into a preferred
drive/UNC/mount path for display and opening. It should not receive a crawler
machine's private mount directory in ordinary result records.

## 2026-09-11 QIndexer Update: Root Path Migration/Duplicate Merge

QIndexer now rewrites existing indexed document paths when an admin changes a
root's canonical path, preserving the previous canonical path as a root alias.
If the new canonical path already has an active row for the same relative item,
the old row is merged away instead of being served as a duplicate. This is meant
to handle moves such as `X:\...` on Windows becoming a service-local Linux/CIFS
mount path while representing the same physical share. Crawl checkpoints are
rewritten at the same time. Fresh Windows and Linux binaries were copied to
`H:\qindexer\bin` for testing.

## 2026-09-12 QIndexer Hardening Update

The service hardening pass changes operational behavior but does not require a
QSurfer adapter change:

- Root aliases in the admin UI are now structured rows (`label`, `platform`,
  `client path`, optional `indexed target`) rather than a delimiter-separated
  text field. Spaces in paths are preserved and `path_aliases` remains the
  same JSON array in the API.
- QIndexer now keeps Windows/UNC identities case-insensitive while preserving
  case for Linux/POSIX paths. Clients should continue sending the configured
  aliases and scope paths exactly as selected.
- Search requests are limited to a 1 MiB body, 100 roots, 100 extensions, and
  100 combined include/exclude scopes. Invalid modified-time filters now return
  a structured `400 invalid_modified_after` or `invalid_modified_before`.
- `sort_direction` is the canonical request field. The protocol fixture and
  API documentation now use it; the service still accepts legacy `direction`
  for compatibility.
- Health returns `status: "degraded"` and `index.ready: false` if SQLite root
  state cannot be read. A healthy process with a usable index remains
  `status: "ok"` and `index.ready: true`.
- Admin root/rule/config changes are persisted atomically before becoming live.
  The crawler and filesystem watcher receive immutable configuration snapshots;
  watcher configuration or root changes trigger a clean watcher rebuild.
- Stop, clear-index, delete-root, and repair now cancel in-flight enrichment
  work as well as directory crawls. An operation waits for that work to drain
  and reports that it is still stopping instead of claiming completion early.
- A stopped root is remembered and `POST /admin/v1/crawler/resume` requeues it
  from its persisted checkpoints. A repair likewise requeues only the roots it
  interrupted after maintenance is released; it no longer leaves a cancelled
  root idle until the next scheduled reconciliation.
- Cancelling work also drains queued content/OCR/hash jobs. This prevents a
  root-path repair from later attempting extraction through an old service
  mount path; refill work is generated from the repaired catalog path instead.
- Admin metrics now include `active_roots`. The management UI presents those
  names with live file, directory, and I/O throughput rather than an invented
  percentage for an unbounded traversal.

Validation from this workspace: `go test ./...`, `go vet ./...`, and Windows
and Linux cross-builds pass. The UI alias change was already present in the
current worktree and was verified to serialize structured `path_aliases`.

## 2026-09-12 QIndexer Update: Repair and Shutdown Responsiveness

- `GET /admin/v1/operations/repair` exposes the current root, phase, alias
  count, rewrite count, duplicate merge count, and timestamps. The web admin
  UI polls it while a repair is active and presents the live phase in the
  Operations strip.
- Repair no longer scans an entire root to test a configured alias. It uses
  the indexed, path-segment-aware prefix to select only records under that
  alias. The embedded-path repair likewise selects only paths containing a
  nested canonical-root signature, leaving normal canonical rows untouched.
  A successful no-op repair should now complete quickly instead of appearing
  to hang at one alias checked.
- Service Stop immediately cancels crawl and enrichment work, returns an
  accepted response, then performs bounded HTTP/watcher cleanup. A blocked
  network file read or third-party parser is no longer allowed to keep the
  process alive past that shutdown window. Client code should treat
  `202 {"status":"stopping"}` as the normal acknowledgement and wait for
  health to become unavailable rather than expecting a synchronous stop.

## 2026-09-12 QIndexer Update: Crawl Priority and SQLite Resilience

- A full crawl is now explicitly two-phase. Phase one indexes the searchable
  namespace (folders, names, paths, sizes, and timestamps). Content extraction,
  OCR, and hashing do not start while any structural crawl is active. They run
  as a secondary pass after the root is queryable.
- Full-crawl metadata writes honor `crawler.index_batch_size`. Worker-local
  documents are committed in bounded SQLite transactions rather than one
  transaction per file. This is important for high-latency SMB/NAS paths.
- Records marked `missing` are rechecked before ordinary traversal on the next
  reachable crawl. A still-absent priority record is not logged as a new error;
  a returned record is restored to `active` immediately.
- A temporarily unreadable child directory produces a `partial` run, preserves
  existing records, clears only traversal checkpoints, and retries rather than
  marking an otherwise reachable root missing.
- Repair uses bounded, committed path-rewrite batches. Interrupting repair can
  leave it incomplete, but cannot leave one giant open SQLite transaction or
  require index reconstruction. The next repair continues idempotently.
- Watcher events remain path-scoped through `CrawlHint`. A changed file is
  reindexed by its file path; a changed directory is limited to its subtree.
  Only a bounded dirty-event overflow or watcher coverage loss escalates to a
  root crawl. Secondary claims sort by newest `indexed_at`, so changed files
  move ahead of the historical extraction/OCR/hash backlog.

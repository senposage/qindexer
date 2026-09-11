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

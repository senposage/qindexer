# QIndexer HTTP Client Guide

QIndexer is an ordinary JSON-over-HTTP service. Any application that can make
authenticated HTTP requests can use it; QSurfer is not required.

## Connection

Set the search API URL and bearer token in the calling process. The service
never accepts a token in a query string.

```bash
export QINDEXER_URL="http://indexer-host:41973"
export QINDEXER_TOKEN="replace-with-search-token"
```

Every search request needs this header:

```http
Authorization: Bearer <search-token>
```

On Windows PowerShell:

```powershell
$baseUrl = "http://indexer-host:41973"
$headers = @{ Authorization = "Bearer $env:QINDEXER_TOKEN" }
```

Do not expose the search or admin endpoint on an untrusted network without TLS
and network access controls.

## Discover The Service

Check liveness and index usability before issuing searches:

```bash
curl -fsS "$QINDEXER_URL/v1/health" \
  -H "Authorization: Bearer $QINDEXER_TOKEN"
```

Read capabilities rather than assuming an optional feature such as OCR or
content search is enabled:

```bash
curl -fsS "$QINDEXER_URL/v1/capabilities" \
  -H "Authorization: Bearer $QINDEXER_TOKEN"
```

The capabilities response supplies `max_page_size`, `max_path_scopes`,
`max_concurrent_searches`, available filters, and supported sort values.

List roots when building a root picker or translating user-visible paths:

```bash
curl -fsS "$QINDEXER_URL/v1/roots" \
  -H "Authorization: Bearer $QINDEXER_TOKEN"
```

Each root provides its canonical service path and optional client aliases.
Treat `root_id` as stable identity. Use `display_path` for UI when provided;
keep canonical `path` for caching and exact comparisons.

## Search Files And Folders

Post JSON to `/v1/search`. This asks for a first page of Office documents in
one root, with a stable caller correlation ID:

```bash
curl -fsS -X POST "$QINDEXER_URL/v1/search" \
  -H "Authorization: Bearer $QINDEXER_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{
    "search_id": "my-client-001",
    "query": "budget q3",
    "filters": {
      "roots": ["finance"],
      "extensions": ["pdf", "docx", "xlsx"],
      "match_fields": ["name", "path", "content"],
      "match_mode": "prefix"
    },
    "limit": 50,
    "offset": 0,
    "sort": "modified",
    "sort_direction": "desc"
  }'
```

Use `has_more` and `next_offset` from the response for the next page. Do not
guess the next offset from the number of visible results:

```bash
curl -fsS -X POST "$QINDEXER_URL/v1/search" \
  -H "Authorization: Bearer $QINDEXER_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"search_id":"my-client-001","query":"budget q3","limit":50,"offset":50,"sort":"modified","sort_direction":"desc"}'
```

Result fields designed for general clients:

- `result_id`, `root_id`, canonical `path`, and `etag` identify a result.
- `display_path` is a client-friendly alias when one matches.
- `kind` and `is_folder` explicitly distinguish files from folders.
- `matched_fields` and `highlights` support rendering matches without client
  query parsing.
- `indexed_at` plus response `index.freshness_seconds` communicate staleness.
- `status` and `access_status` report missing, moved, or inaccessible items.

## Path Scopes

Scopes are path-segment-aware. `C:\Legal` includes `C:\Legal\brief.docx`,
but never `C:\Legalities\notes.docx`.

```json
{
  "query": "motion",
  "filters": {
    "include_paths": ["X:\\Legal\\Open", "X:\\Finance"],
    "exclude_paths": ["X:\\Legal\\Archive"],
    "kind": "file"
  },
  "limit": 100,
  "sort": "relevance",
  "sort_direction": "desc"
}
```

Use root aliases returned by `/v1/roots` when the client sees a mapped drive or
UNC path different from the service host's mount. `scope_aliases` is available
for explicit request-local translation; its shape is documented in
[the API reference](api.md#post-v1search).

## Folder Resolution And Suggestions

Use `/v1/directories` for a real folder-only query. It does not fetch ordinary
results and filter them client-side:

```bash
curl -fsS -X POST "$QINDEXER_URL/v1/directories" \
  -H "Authorization: Bearer $QINDEXER_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"search_id":"folder-lookup-1","query":"Contracts","limit":20,"sort":"name","sort_direction":"asc"}'
```

Use `/v1/suggest` for lightweight indexed path autocomplete. It never touches
the live filesystem:

```bash
curl -fsS -X POST "$QINDEXER_URL/v1/suggest" \
  -H "Authorization: Bearer $QINDEXER_TOKEN" \
  -H "Content-Type: application/json" \
  --data '{"search_id":"address-bar-1","query":"X:\\Legal","limit":15}'
```

## Error Handling

All errors have a structured body:

```json
{
  "error": {
    "code": "search_busy",
    "message": "search capacity is temporarily exhausted",
    "retryable": true,
    "unavailable": false
  }
}
```

Only retry errors where `retryable` is true. A bad query, malformed request, or
unknown root is not retryable. Treat `unavailable: true` as a service-health
condition and re-check `/v1/health` with backoff.

## Full Reference

See [API Reference](api.md) for the complete request and response schema, all
filters, management endpoints, and root-rule payloads. See
[Configuration Reference](configuration.md) for server and crawler settings.

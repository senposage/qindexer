# QIndexer API Reference

Default endpoints:

- Search API: `http://127.0.0.1:41973/v1`
- Admin API: `http://127.0.0.1:41974/admin/v1`
- Admin UI: `http://127.0.0.1:41974/`

Both APIs use bearer tokens when configured:

```http
Authorization: Bearer <token>
```

The management UI itself is safe to open without a token. It calls the public
`GET /admin/v1/status` endpoint, which reveals only service health, version,
index readiness, document count, and whether first-run administration has been
configured. All root, path, rule, activity, log, metric, and configuration
endpoints require the admin token.

Structured errors:

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

`retryable` is true for rate limits and server errors. `unavailable` is true
for service-unavailable responses.

## Search API

### GET /v1/health

Returns service liveness, build identity, generation, and index usability.

```json
{
  "status": "ok",
  "protocol_version": "1.1",
  "service_instance": "uuid",
  "version": "1.0.0",
  "commit": "local",
  "generation": 42,
  "index": {
    "ready": true,
    "document_count": 12345
  }
}
```

### GET /v1/capabilities

Declares feature support, filters, sorts, and request limits.

Important features:

- `metadata_search`
- `content_search`
- `ocr_search`
- `content_hashing`
- `crawl_control`
- `folder_search`
- `folder_suggest`
- `pagination`
- `sorting`
- `path_scopes`
- `root_filtering`
- `exclusions`
- `exact_match`
- `structured_boolean`

Important limits:

- `max_page_size`
- `max_scope_paths`
- `max_concurrent_searches`
- `supported_sorts`

### GET /v1/roots

Returns friendly root metadata for root pickers, scope resolution, and index
freshness display.

Fields include:

- `root_id`
- `name`
- `labels`
- `canonical_path`
- `aliases`
- `available`
- `document_count`
- `missing_count`
- `last_status`
- `last_error`
- `last_successful_crawl_at`

Aliases include:

```json
{
  "alias_id": "finance-x",
  "platform": "windows-drive",
  "path": "X:\\Finance",
  "target": "/mnt/finance",
  "canonical": false
}
```

`target` is optional. It is used when an alias maps to a canonical subfolder.

### POST /v1/search

Searches indexed files and folders.

```json
{
  "search_id": "client-correlation-id",
  "query": "budget q3",
  "filters": {
    "roots": ["finance"],
    "extensions": ["pdf", "docx"],
    "path_prefix": "X:\\Finance",
    "path_prefixes": ["X:\\Finance", "X:\\Legal"],
    "include_paths": ["X:\\Finance\\Open"],
    "exclude_paths": ["X:\\Finance\\Archive"],
    "scope_aliases": [
      {
        "platform": "windows-drive",
        "path": "X:\\",
        "target": "\\\\nas01\\Shared"
      }
    ],
    "kind": "file",
    "modified_after": "2026-01-01T00:00:00Z",
    "modified_before": "2026-12-31T23:59:59Z",
    "min_size": 1024,
    "max_size": 10485760,
    "match_fields": ["name", "path", "extension", "content"],
    "match_mode": "prefix",
    "boolean": {
      "all": ["budget"],
      "any": ["forecast", "plan"],
      "not": ["draft"]
    }
  },
  "limit": 50,
  "offset": 0,
  "sort": "modified",
  "sort_direction": "desc"
}
```

Request fields:

- `search_id`: echoed back for logs and cancellation correlation.
- `query`: free text FTS query. Raw query text is not path-alias rewritten.
- `limit`: page size, capped by capabilities.
- `offset`: result offset for paging.
- `sort`: `name`, `modified`, `size`, or `relevance`.
- `sort_direction`: `asc` or `desc`.

Filters:

- `roots`: restrict to root IDs.
- `extensions`: extension allow-list for this search.
- `path_prefix`: legacy single path scope.
- `path_prefixes`: multiple include scopes.
- `include_paths`: equivalent to `path_prefixes`; preferred for QSurfer scoped
  folder selection.
- `exclude_paths`: multiple excluded scopes.
- `scope_aliases`: request-local path translations supplied by a client.
- `kind`: `file` or `folder`.
- `modified_after` / `modified_before`: RFC3339 time bounds.
- `min_size` / `max_size`: byte bounds.
- `match_fields`: any of `name`, `path`, `extension`, `content`.
- `match_mode`: `prefix` (default) matches the beginning of indexed tokens;
  `exact` matches an exact token or exact multi-word phrase. It applies to any
  selected `match_fields` combination, including content-only search.
- `boolean`: optional structured Boolean terms. `all` terms are joined with
  AND; `any` terms are joined with OR as a group; `not` terms are excluded.
  Terms are always escaped by QIndexer and combine with `query`, field
  selection, scopes, sorts, and pagination. At least one `query`, `all`, or
  `any` term is required when using `not`.

The `query` string is always literal search text; it does not accept FTS or
Boolean syntax. Use the structured `boolean` filter instead.

Path scope matching is segment-aware. `C:\Legal` matches `C:\Legal\brief.docx`
but not `C:\Legalities\brief.docx`.

Folders only match by their own folder name. A child folder is not returned just
because an ancestor folder matched the query.

Response:

```json
{
  "search_id": "client-correlation-id",
  "offset": 0,
  "has_more": true,
  "next_offset": 50,
  "took_ms": 8,
  "results": [
    {
      "id": "uuid",
      "result_id": "uuid",
      "root_id": "finance",
      "path": "/mnt/finance/reports/q3-budget.docx",
      "display_path": "X:\\Finance\\reports\\q3-budget.docx",
      "normalized_path": "/mnt/finance/reports/q3-budget.docx",
      "name": "q3-budget.docx",
      "extension": "docx",
      "kind": "file",
      "is_folder": false,
      "etag": "123:2026-09-11T12:00:00Z",
      "signature": "123:2026-09-11T12:00:00Z",
      "size": 123,
      "modified_at": "2026-09-11T12:00:00Z",
      "indexed_at": "2026-09-11T12:01:00Z",
      "status": "active",
      "access_status": "metadata_readable",
      "owner": "DOMAIN\\user",
      "content_status": "extracted",
      "ocr_status": "not_requested",
      "hash_status": "hashed",
      "content_hash": "sha256...",
      "matched_fields": ["name", "path"],
      "highlights": {
        "name": ["q3-budget.docx"]
      }
    }
  ],
  "index": {
    "ready": true,
    "generation": 42,
    "freshness_seconds": 120,
    "roots": [
      {
        "root_id": "finance",
        "status": "ok",
        "generation": 42,
        "last_successful_crawl_at": "2026-09-11T12:01:00Z",
        "freshness_seconds": 120
      }
    ]
  }
}
```

`path` is the stable canonical service path. `display_path` is optional and
client-friendly. QSurfer should display `display_path || path` while using
canonical `path`, `root_id`, `result_id`, and `etag` for identity/cache logic.

### POST /v1/directories

Same request/response shape as search, but only returns folders.

Use this for typed folder scope resolution and address autocomplete when the
client needs folder-only results.

### POST /v1/suggest

Same request shape as search, capped for lightweight folder/path suggestions.
It never crawls the live filesystem.

## Admin API

### GET /admin/v1/status

Unauthenticated, service-level status for the locked admin UI. It intentionally
does not return root count, root IDs/names/paths, crawler activity, logs,
configuration, or index size.

```json
{
  "status": "ok",
  "protocol_version": "1.1",
  "service_instance": "uuid",
  "version": "1.1.0",
  "admin_configured": true,
  "index": {"ready": true, "document_count": 12345}
}
```

### POST /admin/v1/bootstrap

Localhost-only first-run token setup:

```json
{"token":"at-least-sixteen-chars"}
```

### GET /admin/v1/service

Admin-authenticated health view.

### GET /admin/v1/config

Returns effective configuration with tokens redacted.

### GET /admin/v1/config/export

Exports index, crawler, watcher, and root settings. Secrets and token paths are
excluded.

### POST /admin/v1/config/import

Imports an exported config backup. Current search/admin auth is preserved.

### POST /admin/v1/config/validate

Validates the active config.

### GET /admin/v1/metrics

Returns crawler rates, I/O rate, active state, pause/throttle state, and
`index_size_bytes`.

### GET /admin/v1/diagnostics

Returns redacted config, service info, root states, recent crawls, crawler
metrics, and log availability.

### POST /admin/v1/crawler/pause

```json
{"seconds":300}
```

Pauses crawler work for up to 24 hours.

### POST /admin/v1/crawler/resume

Clears manual pause.

### POST /admin/v1/service/stop

Cancels active crawls, waits briefly for checkpoints/catalog writes, then
shuts the process down through the service lifecycle.

### PUT /admin/v1/crawler/settings

Updates global crawler resource and enrichment settings:

```json
{
  "collect_ownership": true,
  "adaptive_throttle": {
    "enabled": true,
    "sample_interval_seconds": 5,
    "cpu_percent_threshold": 80,
    "disk_busy_percent_threshold": 70,
    "recovery_samples": 3
  },
  "content_extraction": {
    "enabled": true,
    "worker_count": 1,
    "queue_size": 1000,
    "max_file_size_mb": 64,
    "max_stored_text_kb": 1024
  },
  "ocr": {
    "enabled": true,
    "engine": "auto",
    "tesseract_command": "tesseract",
    "ocrmypdf_command": "ocrmypdf",
    "languages": "eng",
    "worker_count": 1,
    "queue_size": 100,
    "max_file_size_mb": 128,
    "timeout_seconds": 180
  },
  "hashing": {
    "enabled": true,
    "worker_count": 1,
    "queue_size": 1000,
    "max_file_size_mb": 2048
  }
}
```

### POST /admin/v1/index/compact

Runs a storage-maintenance pass. QIndexer checkpoints and pauses active crawls,
trims legacy document text above `max_stored_text_kb`, optimizes FTS, and runs
SQLite `VACUUM` to return unused pages to the filesystem. Crawls that were
active before maintenance are scheduled again afterward.

The operation can take time and SQLite may need temporary free disk space while
rewriting the database. It does not read, change, or delete source files.

Response:

```json
{
  "status": "compacted",
  "result": {
    "trimmed_documents": 14,
    "trimmed_text_bytes": 7340032,
    "before": {"database_bytes": 9000000000, "free_bytes": 2000000000},
    "after": {"database_bytes": 5100000000, "free_bytes": 0}
  }
}
```

### PUT /admin/v1/network

Updates search/admin bind addresses and public URL. A restart is required.

### GET /admin/v1/roots

Admin-authenticated root list.

### POST /admin/v1/roots

Creates a root. Payload matches root rules.

### PUT /admin/v1/roots/{root_id}/rules

Updates all rules for one root. The full payload is accepted:

```json
{
  "id": "finance",
  "name": "Finance share",
  "path": "\\\\nas01\\finance\\*",
  "enabled": true,
  "labels": ["nas", "finance"],
  "credential_ref": "os-service-account",
  "path_aliases": [
    {
      "id": "finance-x",
      "platform": "windows-drive",
      "path": "X:\\Finance",
      "target": "\\\\nas01\\finance"
    }
  ],
  "include_extensions": ["pdf", "docx"],
  "exclude_extensions": ["tmp", "bak"],
  "include_file_patterns": ["*.pdf"],
  "exclude_file_patterns": ["~$*", "*.tmp"],
  "include_folder_patterns": ["**/Finance/**"],
  "exclude_folder_patterns": ["**/.git/**"],
  "exclude_patterns": [],
  "content_extraction": true,
  "ocr": false,
  "hashing": true,
  "collect_ownership": true
}
```

When the canonical root path changes, QIndexer rewrites existing indexed rows
from the previous path to the new path and preserves the previous path as an
alias.

### POST /admin/v1/roots/{root_id}/validate

Checks whether the root path can be expanded/reached.

### POST /admin/v1/roots/{root_id}/crawl

Schedules a manual crawl for one root.

### POST /admin/v1/roots/{root_id}/clear-index

```json
{"confirm_root_id":"finance"}
```

Clears documents, FTS rows, checkpoints, crawl history, and root state for one
root. Source files are never deleted.

### POST /admin/v1/roots/{root_id}/repair-index

Repairs path identity for one root:

- Rewrites alias-prefixed rows into the canonical root path.
- Supports aliases with optional canonical `target`.
- Merges duplicate rows when the canonical row already exists.
- Repairs UNC/machine-prefixed wrappers around the canonical root path.
- Rewrites crawl checkpoints at the same time.
- Does not modify source files.

Response:

```json
{
  "status": "repaired",
  "root_id": "finance",
  "aliases_checked": 2,
  "paths_rewritten": 100,
  "duplicate_paths_merged": 12,
  "embedded_paths_rewritten": 8,
  "embedded_paths_merged": 4,
  "aliases_repaired": ["X:\\Finance"]
}
```

### DELETE /admin/v1/roots/{root_id}

```json
{"confirm_root_id":"finance"}
```

Cancels an active crawl for the root and shared enrichment work, waits for them
to drain, removes the root from config, then marks its indexed rows as
deleted/no-op for serving. If the bounded wait expires, the endpoint returns
the retryable `root_still_stopping` conflict and changes nothing. Cleanup is
deferred. Source files are never deleted.

### GET /admin/v1/crawls

Returns recent crawl activity.

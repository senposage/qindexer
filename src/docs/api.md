# QIndexer API

Base endpoints:

- Search API: `http://127.0.0.1:41973/v1`
- Admin API: `http://127.0.0.1:41974/admin/v1`
- Admin web UI: `http://127.0.0.1:41974/`

Both APIs use bearer tokens when configured:

```http
Authorization: Bearer <token>
```

## Search API

All search-facing endpoints use protocol version `1.1`. Health and capabilities include the service instance, build version, and current index generation. Errors always use this shape:

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

### Health

```http
GET /v1/health
```

### Capabilities

```http
GET /v1/capabilities
```

### Roots

```http
GET /v1/roots
```

Each root has a stable `root_id`, its service-local `canonical_path`, and an `aliases` list. An alias is `{ "alias_id", "platform", "path" }`; `alias_id` is configured when possible, otherwise deterministically derived from the root and alias path. Clients should use this response to recognize that mapped drives, UNC shares, and local mounts represent the same root. Free-text `query` is never rewritten; only an explicit folder scope may later be translated through an advertised alias.

### Search

```http
POST /v1/search
Content-Type: application/json
```

```json
{
  "search_id": "qsurfer-request-42",
  "query": "budget q3",
  "filters": {
    "roots": ["local-sample"],
    "extensions": ["pdf", "docx"],
    "path_prefixes": ["C:\\Shares\\Finance", "C:\\Shares\\Legal"],
    "exclude_paths": ["C:\\Shares\\Legal\\Archive"],
    "kind": "file",
    "modified_after": "2026-01-01T00:00:00Z",
    "modified_before": "2026-12-31T23:59:59Z",
    "min_size": 1024,
    "max_size": 10485760
  },
  "limit": 50,
  "offset": 0,
  "sort": "modified",
  "sort_direction": "desc"
}
```

`path_prefix` remains supported for compatibility. `path_prefixes` and `include_paths` are equivalent multi-scope include fields; `exclude_paths` removes scopes. Prefix matching is path-segment-aware, so `C:\Legal` matches `C:\Legal\brief.docx` but not `C:\Legalities\brief.docx`.

Search responses echo `search_id` and include `offset`, `has_more`, and `next_offset` for progressive paging. Results always contain `result_id`, `etag`, `root_id`, canonical `path`, `kind`, `is_folder`, `indexed_at`, and filename/path match metadata. The response `index` object includes per-root crawl status, generation, and freshness in seconds.

### Directories and suggestions

```http
POST /v1/directories
POST /v1/suggest
```

Both accept the same request shape as search and return the same response shape, but only return indexed folders. `suggest` is capped at 25 results for address-bar autocomplete. These endpoints never inspect the live filesystem.

### Limits

Read `GET /v1/capabilities` before integrating a client. It declares supported filters/sorts, max page size, the 100-path scope ceiling, and the bounded concurrent-search capacity.

The shared QSurfer provider fixture is [qsurfer-search-v1.json](../contracts/qsurfer-search-v1.json). Both providers should validate the required response/result fields and the path-segment scope rule against it.

## Admin API

### Portable configuration backup and diagnostics

```http
GET /admin/v1/config/export
POST /admin/v1/config/import
GET /admin/v1/diagnostics
```

Export/import carries index, crawler, watcher, and root settings, including root path aliases. Authentication tokens and token-file paths are deliberately excluded. Import preserves the current service and management authentication settings and returns `restart_recommended: true` when the changed settings require process restart.

Diagnostics returns the redacted backup, current service/index generation, root states, recent crawl runs, and metrics. It explicitly reports whether a file log sink is configured.

### Service

```http
GET /admin/v1/service
```

### Config View

```http
GET /admin/v1/config
```

Tokens are redacted.

### Validate Config

```http
POST /admin/v1/config/validate
```

### Validate Root Path

```http
POST /admin/v1/roots/{root_id}/validate
```

### Update Root Rules

```http
PUT /admin/v1/roots/{root_id}/rules
Content-Type: application/json
```

```json
{
  "path": "\\\\nas01\\finance\\*",
  "enabled": true,
  "labels": ["nas", "finance"],
  "credential_ref": "os-service-account",
  "include_extensions": ["pdf", "docx", "xlsx"],
  "exclude_extensions": ["tmp", "bak"],
  "include_file_patterns": ["*.pdf", "report-*.xlsx"],
  "exclude_file_patterns": ["~$*", "*.tmp"],
  "include_folder_patterns": ["**/Finance/**"],
  "exclude_folder_patterns": ["**/.git/**", "**/node_modules/**"],
  "exclude_patterns": []
}
```

Rules are persisted to the YAML config. Secret values are not managed through this endpoint.

Root paths may be literal paths or filesystem glob patterns. Examples:

- `D:\Shares`
- `X:\*`
- `\\nas01\finance\*`
- `/mnt/nas-finance/*`

### Trigger Crawl

```http
POST /admin/v1/roots/{root_id}/crawl
```

### Recent Crawls

```http
GET /admin/v1/crawls
```

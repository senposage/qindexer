# Configuration Reference

QIndexer reads YAML configuration from `--config`. Relative paths are resolved
from the config file location.

## Server

```yaml
server:
  bind: "127.0.0.1:41973"
  bind_addresses:
    - "127.0.0.1:41973"
  public_base_url: "http://127.0.0.1:41973"
  auth:
    mode: "static-token"
    token: ""
    token_file: ""
```

- `bind`: legacy single search API bind address.
- `bind_addresses`: one or more search API `host:port` listeners. If empty,
  QIndexer uses `bind`.
- `public_base_url`: URL advertised to clients.
- `auth.mode`: `static-token` enables bearer auth. Empty token on first run
  requires local UI bootstrap.
- `auth.token`: inline bearer token. Avoid committing real tokens.
- `auth.token_file`: file containing the token. Preferred for deployments.

## Management

```yaml
management:
  enabled: true
  bind: "127.0.0.1:41974"
  bind_addresses:
    - "127.0.0.1:41974"
  auth:
    mode: "admin-token"
    token: ""
    token_file: ""
```

- `enabled`: starts the admin API and web UI.
- `bind` and `bind_addresses`: same behavior as `server`, but for admin.
- `auth`: same token fields as `server`. First-run bootstrap sets the admin
  token and, if missing, the search token too.

## Index

```yaml
index:
  data_dir: "data"
  commit_interval_seconds: 5
  max_results: 200
```

- `data_dir`: SQLite database directory.
- `commit_interval_seconds`: reserved for future catalog batching; it does not
  alter current write behavior.
- `max_results`: server-side maximum page size.

QIndexer stores `qsurfer-search.db`, `qsurfer-search.db-wal`, and
`qsurfer-search.db-shm` in `data_dir`.

## Crawler

```yaml
crawler:
  scan_interval_seconds: 300
  root_parallelism: 1
  directory_worker_count: 4
  metadata_worker_count: 8
  metadata_queue_size: 5000
  index_batch_size: 1000
  ignore_hidden: false
  follow_symlinks: false
  collect_ownership: false
  missing_after_successful_crawls: 3
```

- `scan_interval_seconds`: periodic reconciliation interval.
- `root_parallelism`: number of roots crawled at once.
- `directory_worker_count`: directory traversal concurrency per crawl.
- `metadata_worker_count`: file stat/enrichment scheduling concurrency.
- `metadata_queue_size`: bounded file queue.
- `index_batch_size`: maximum number of metadata records committed together.
  Metadata workers split this budget so the total in-flight write batch stays
  bounded while high-latency shares do not pay one SQLite transaction per file.
- `ignore_hidden`: skips hidden files where supported.
- `follow_symlinks`: follows symlinked directories when true.
- `collect_ownership`: default owner metadata collection.
- `missing_after_successful_crawls`: successful crawls required before a
  previously indexed missing item is marked missing/deleted.

## Adaptive Throttle

```yaml
adaptive_throttle:
  enabled: true
  sample_interval_seconds: 5
  cpu_percent_threshold: 80
  disk_busy_percent_threshold: 70
  recovery_samples: 3
```

When enabled, QIndexer pauses crawl and enrichment work while host CPU or disk
pressure is above threshold. QIndexer subtracts its own CPU share when
evaluating host pressure, so the thresholds reflect other workload on the
workstation or NAS. The management UI can change all five settings live.

- `enabled`: turns adaptive pressure pausing on or off.
- `sample_interval_seconds`: system sampling interval; valid range `1`-`3600`.
- `cpu_percent_threshold`: external CPU percentage that pauses work; valid
  range greater than `0` through `100`.
- `disk_busy_percent_threshold`: busiest observed local disk busy percentage
  that pauses work; valid range greater than `0` through `100`.
- `recovery_samples`: consecutive healthy samples required before work resumes.

## Pause Windows

```yaml
pause_windows:
  - start: "08:30"
    end: "18:00"
    days: ["mon", "tue", "wed", "thu", "fri"]
```

Pause windows suppress crawler work during known busy periods. Times are local
to the service host. `days` accepts `mon`, `tue`, `wed`, `thu`, `fri`, `sat`,
and `sun`.

## Content Extraction

```yaml
content_extraction:
  enabled: false
  worker_count: 1
  queue_size: 1000
  max_file_size_mb: 64
  max_stored_text_kb: 1024
```

When enabled, supported document text is extracted into the index. Current
content extraction is local and best effort. Supported inputs include plain
text, Markdown, CSV, PDF text, and common Office formats supported by the
extractor implementation.

`max_stored_text_kb` caps normalized text retained for each document. The
default is 1024 KiB and the accepted range is 64-1024 KiB. This limit applies
to ordinary extraction and OCR; it keeps a pathological or unusually verbose
document from inflating the SQLite index.

## OCR

```yaml
ocr:
  enabled: false
  engine: "auto"
  tesseract_command: "tesseract"
  ocrmypdf_command: "ocrmypdf"
  languages: "eng"
  worker_count: 1
  queue_size: 100
  max_file_size_mb: 128
  timeout_seconds: 180
```

- `engine`: `auto`, `tesseract`, or `ocrmypdf`.
- `tesseract_command`: command or absolute path for Tesseract.
- `ocrmypdf_command`: command or absolute path for OCRmyPDF.
- `languages`: OCR language list such as `eng`.
- `timeout_seconds`: per-file OCR timeout.

OCR never modifies source files. In `auto` mode, images use Tesseract and
scanned PDFs use OCRmyPDF after ordinary text extraction finds no text.

## Hashing

```yaml
hashing:
  enabled: false
  worker_count: 1
  queue_size: 1000
  max_file_size_mb: 2048
```

When enabled, QIndexer records SHA-256 hashes for duplicate detection and later
similar-copy workflows.

## Watcher

```yaml
watcher:
  enabled: true
  debounce_milliseconds: 1500
  max_watched_directories: 25000
  max_dirty_paths_per_flush: 500
  activity_half_life_days: 60
  rebalance_minutes: 15
```

The watcher uses OS filesystem hints when available. It is an accelerator, not
the only source of truth. Periodic full crawls reconcile missed events.

On large NAS trees, the watcher pins roots and immediate child folders, then
uses the remaining watch budget for folders with recent change activity. Folder
activity persists across restarts and decays with `activity_half_life_days`;
the default 60-day half-life intentionally keeps day-to-day working folders
hot for a long time. `rebalance_minutes` controls how often cold watches are
replaced by hotter candidates. `max_watched_directories` is the total OS watch
budget across all roots. It can be changed live from the admin UI; QIndexer
restarts only the watcher and preserves crawl/index work.

## Roots

```yaml
roots:
  - id: "finance"
    name: "Finance share"
    path: "\\\\nas01\\finance\\*"
    enabled: true
    labels: ["nas", "finance"]
    credential_ref: "os-service-account"
```

- `id`: stable root ID returned to clients.
- `name`: optional friendly display name.
- `path`: canonical service path or wildcard root.
- `enabled`: disabled roots remain configured but are not crawled.
- `labels`: free-form tags surfaced through the API/UI.
- `credential_ref`: operator note naming the OS/NAS credential source. QIndexer
  does not store secrets for roots.

Root paths may be:

- `D:\Shares`
- `X:\*`
- `\\nas01\finance`
- `\\nas01\finance\*`
- `/mnt/nas-finance`
- `/mnt/nas-*/*`

## Root Aliases

```yaml
path_aliases:
  - id: "finance-x"
    platform: "windows-drive"
    path: "X:\\Finance"
  - id: "finance-linux"
    platform: "linux"
    path: "/mnt/finance"
  - id: "legal-x"
    platform: "windows-drive"
    path: "X:\\Legal"
    target: "/mnt/team-share/Legal"
```

- `id`: optional stable alias ID. QIndexer derives one if omitted.
- `platform`: operator-facing label such as `windows-drive`, `windows-unc`,
  `linux`, `mac`, or `service`.
- `path`: client-facing alias prefix.
- `target`: optional canonical path prefix when the alias maps to a subfolder
  rather than the root itself.

Aliases are used for explicit path scopes, `display_path`, and repair. Free
text search terms are not rewritten.

## Root Rules

Each root owns its own rules.

```yaml
include_extensions: ["pdf", "docx", "xlsx"]
exclude_extensions: ["tmp", "bak", "partial"]
include_file_patterns:
  - "*.pdf"
  - "report-*.xlsx"
exclude_file_patterns:
  - "~$*"
  - "*.tmp"
include_folder_patterns:
  - "**/Finance/**"
exclude_folder_patterns:
  - "**/.git/**"
  - "**/node_modules/**"
  - "**/@Recently-Snapshot/**"
  - "**/$RECYCLE.BIN/**"
exclude_patterns: []
```

- `include_extensions`: allow only these extensions. Empty means all.
- `exclude_extensions`: block these extensions.
- `include_file_patterns`: wildcard file-name/path includes. Empty means all.
- `exclude_file_patterns`: wildcard file-name/path excludes.
- `include_folder_patterns`: wildcard folder includes. Empty means all.
- `exclude_folder_patterns`: wildcard folder excludes.
- `exclude_patterns`: legacy compatibility exclusions.

A bare folder name, such as `.Trash-1000`, matches that directory at any depth
of the root. Use `**/name/**` when you want to express the same intent as an
explicit folder path pattern, including its descendants.

Rules are evaluated before catalog insertion. Exclusions prevent future crawl
insertion; use root repair/clear/reconciliation to clean older rows.

Recommended default folder exclusions for office/NAS roots:

- `**/.git/**`
- `**/node_modules/**`
- `**/tmp/**`
- `**/@Recently-Snapshot/**`
- `**/@Recycle/**`
- `**/#recycle/**`
- `**/$RECYCLE.BIN/**`
- `**/RECYCLER/**`
- `**/.sync/**`
- `**/.qsync/**`
- `**/.qsync_sn/**`

## Per-Root Enrichment Switches

```yaml
content_extraction: true
ocr: true
hashing: true
collect_ownership: true
```

These booleans override crawler-wide defaults for the root. Worker counts,
queues, size limits, command paths, and timeouts remain global.

## Backup And Secrets

Admin export/import includes index, crawler, watcher, and roots. Tokens and
token file paths are redacted and preserved from the receiving service. Keep
operator configs with real tokens outside version control.

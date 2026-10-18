# Changelog

## 1.3.0 - 2026-10-18

QIndexer 1.3 consolidates the 1.2 development series into a stable indexing,
search, and deployment baseline.

### Catalog, Search, and Storage

- PostgreSQL is the default catalog for concurrent indexing and search, with
  SQLite retained as an explicit portable option.
- Search supports normalized results, stable IDs, path aliases, root filtering,
  paging, sorts, field matches, content search, exact match mode, suggestions,
  folder-only search, and structured errors for QSurfer and other clients.
- Catalog metrics, maintenance, compacting, and PostgreSQL full reclaim are
  available through the authenticated admin UI. Large text is bounded,
  normalized, and replaced in place to control index growth.

### Crawling and Reliability

- Host-aware, per-root crawl limits use measured storage latency so local NVMe
  roots can run quickly while SMB, NAS, and VPN roots remain conservative.
- Crawls checkpoint safely, resume due work after downtime, protect temporary
  or inaccessible paths from being marked missing, and recover cleanly from
  interrupted enrichment claims.
- Root delete, clear, repair, pause, stop, and shutdown coordinate active work
  without corrupting the catalog or disrupting unrelated roots.
- Adaptive CPU and disk pressure controls pause traversal and enrichment after
  sustained external load, then recover after healthy samples.

### Watcher and Enrichment

- Filesystem notifications are debounced, coalesced per root, cooldown-limited,
  and batched before database work. Watchers rebalance toward hot folders and
  recover after unavailable shares or notification handles.
- Excluded files never create hints or activity records. Excluded folders and
  their descendants never consume watcher capacity, become hot folders, or run
  targeted crawls; excluding a file does not disable its parent directory.
- Content extraction, OCR, and SHA-256 hashing use independent worker pools,
  bounded retry lanes, per-root overrides, safe parser-panic containment, and
  explicit status/error reporting. PDFs and images route directly to OCR when
  appropriate.

### Administration and Deployment

- The responsive admin UI adds authenticated/locked states, live operational
  metrics, log filtering, advanced JSON configuration editing, repair progress,
  dark mode, and clear save/loading feedback.
- Windows and Linux bundles include examples, setup/service helpers, optional
  self-contained OCR sidecar installation, private loopback PostgreSQL setup,
  and updater progress reporting compatible with Windows PowerShell 5.1.
- QSurfer-managed Linux installs generate protected inline service/admin
  tokens and an empty `roots:` section that QSurfer can seed with local
  volumes, while QIndexer remains usable when no roots are supplied.

### Default Hygiene Rules

- New configurations and new roots receive an editable, cross-platform
  default set for Office/LibreOffice lock files, temporary downloads, Steam
  `.temp_write*` files, editor swaps, OS metadata, recycle bins, snapshots,
  sync metadata, browser/build/package caches, AppData, and virtual/runtime
  trees.
- The default set intentionally does not hide ordinary documents, source,
  archives, logs, executables, or media by extension. See
  `src/configs/example.yaml` for the complete list.

## 1.1 Series

- Introduced authenticated first-run setup, adaptive throttling, standalone
  service setup, sidecar deployment, OCR progress, and safer retry handling.
- Added cross-platform path aliases, root maintenance, missing-record cleanup,
  incremental filesystem watching, and QSurfer-oriented HTTP documentation.

## 1.0 Series

- Established the portable Go service, multi-root filesystem catalog, admin
  UI, content/OCR/hash enrichment, alias-aware path presentation, and the
  core QSurfer-compatible search protocol.

## 1.0.0

First public QIndexer release for QSurfer-compatible filesystem indexing.

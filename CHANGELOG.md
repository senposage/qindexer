# Changelog

## 1.1.0

### Added

- Public, minimal admin status endpoint for a locked UI: service health,
  version, index readiness, document count, and setup state only.
- Configurable adaptive crawler throttle in the management UI and API:
  enablement, CPU threshold, disk-busy threshold, sample interval, and
  recovery samples.
- Standalone HTTP client guide with raw requests for health, capabilities,
  roots, search, paging, folder search, and suggestions.

### Changed

- Admin UI now explicitly reports its locked state and removes all private root
  details, paths, rules, crawl activity, logs, settings, and runtime metrics
  until a valid admin token is accepted.
- Empty-root crawl passes are debug-level diagnostics with configured-root
  count, rather than visible informational noise.
- Adaptive throttle values are range-validated before configuration is saved.
- Documentation now reflects active metadata write batching.

## 1.0.0

First public QIndexer release for QSurfer-compatible filesystem indexing.

### Added

- Portable Go service for Windows and Linux.
- SQLite-backed file and folder catalog with FTS search.
- Authenticated search API and authenticated admin API.
- Local web admin UI for bootstrap, roots, rules, crawler controls, network settings, repair, diagnostics, and index size.
- First-run admin token setup from the local UI.
- Multiple listening interfaces for the search and management APIs.
- Root-scoped include/exclude rules for extensions, files, folders, and legacy path patterns.
- Windows drive, UNC, Linux mount, and wildcard root support.
- Root aliases with optional canonical targets for cross-machine path translation.
- Search pagination, sorting, root filtering, scoped include/exclude paths, folder-only search, and suggest/autocomplete.
- Stable result IDs, etags/signatures, canonical paths, display paths, result kind, access status, indexed timestamps, highlights, and matched fields.
- Optional Office/PDF/text content extraction.
- Optional OCR through Tesseract and OCRmyPDF.
- Optional SHA-256 hashing and owner metadata collection.
- Incremental filesystem watcher with periodic full reconciliation crawls.
- Adaptive CPU/disk throttling, manual pause/resume, root repair, clear index, delete root, and service stop.
- Root path rewrite/merge repair for moved roots, mapped drives, mounted shares, and UNC-wrapped canonical paths.

### Notes

QIndexer indexes and reports filesystem metadata. It does not enforce file access policy. Access remains controlled by the host OS, service account, NAS, and share permissions.

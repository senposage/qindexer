# Changelog

## 1.0.11

### Fixed

- The authenticated admin UI can now start OCR sidecar setup through a LAN
  address; it is no longer incorrectly restricted to loopback-only requests.

## 1.0.10

### Fixed

- Windows OCR setup now resolves the current upstream Tesseract GitHub release
  at install time instead of relying on a per-user `winget` registration.
- Existing OCRmyPDF installations are reused; only missing dependencies are
  installed.
- Sidecar setup distinguishes an interactive UAC request from a Windows
  service in Session 0, where it runs directly with the service's privileges.
- The admin UI reports installer progress and failures, while the installer
  writes its own transcript for diagnosis.

## 1.0.9

### Fixed

- Windows reparse points and legacy profile junctions are skipped before they
  can enter the crawler queue.
- Built-in exclusions now cover non-indexable system and volatile locations,
  including System Volume Information, recycle folders, Windows WebCache, SRU,
  and generated `_MEI*` temporary folders.
- A denied or disappearing child directory now produces a degraded crawl that
  preserves existing index records and waits for the normal next crawl. It no
  longer starts a 30-second whole-root retry or marks unseen files missing.
- Cancelled admin metric requests no longer add misleading catalog warnings to
  the service log.

## 1.0.8

### Fixed

- OCR sidecar setup is now fully automatic: it never requires a QIndexer
  config path and installs the sidecar executables into the system PATH.
- The admin OCR installer resolves helpers in both installed and portable
  release-bundle layouts.

## 1.0.7

### Added

- OCR dependency setup from the local admin settings. When Tesseract or
  OCRmyPDF is unavailable, the UI can launch the packaged installer helper,
  with UAC on Windows and polkit on Linux.
- Windows and Linux deployment scripts now install, verify, and configure
  Tesseract, Ghostscript, OCRmyPDF, and Poppler sidecars. Release bundles
  include the corresponding deployment helpers and example configurations.

### Changed

- OCR stays disabled unless explicitly enabled, avoiding an accidental backlog.

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

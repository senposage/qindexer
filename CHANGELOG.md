# Changelog

## 1.1.8 - 2026-10-12

- Keep filesystem change notifications visible and self-healing: unavailable native watcher handles now report the reason in the admin UI and retry every 30 seconds.
- Schedule content extraction, OCR, and SHA-256 hashing per root, so an active structural crawl on one root does not block enrichment work on another.
- Add an authenticated Clear history action for recent crawl activity. It removes display history only and preserves roots, checkpoints, and indexed documents.

## 1.1.7 - 2026-10-12

- Raise local-disk crawl ceilings and select an observed per-root concurrency cap from a brief directory-latency probe, keeping slow SMB/VPN roots from monopolizing workers.
- Run content extraction, OCR, and SHA-256 hashing in independently configurable worker pools; the admin UI now exposes all crawl and enrichment worker limits.
- Show the active root's measured concurrency profile in the operations summary and document local versus slow-share tuning.

## 1.1.6 - 2026-10-12

- Streamline the admin login header: authenticated sessions hide the token input and use a Logout control; sign-out clears private dashboard state immediately.

## 1.1.5 - 2026-10-12

- Replace a previously recovered Windows OCR helper that still contains the obsolete Tesseract Authenticode check.
- Remove the remaining Authenticode check from Python sidecar setup.

## 1.1.4 - 2026-10-12

- Allow the official upstream Tesseract Windows installer to run without an Authenticode signature check.

## 1.1.3 - 2026-10-12

- Make OCR sidecar deployment self-contained for managed installs: when the helper script is absent beside QIndexer, the service retrieves the release-matched script into its own `scripts` directory before executing it.
- Log each OCR installer request before helper preparation, so a rejected or failed request is diagnosable from the operational log.

## 1.1.2 - 2026-10-12

- Run the packaged OCR dependency installer directly under the QIndexer service identity for authenticated admin requests, regardless of whether the dashboard is reached locally or remotely.
- Clarify OCR installation progress in the admin UI.

## 1.1.1

### Fixed

- Content extraction and OCR failures now use a bounded, delayed retry lane.
  A transient failure is retried twice without blocking normal enrichment work;
  malformed, encrypted, invalid, or unsupported files are marked
  `unindexable` immediately.
- Search responses now expose retained `content_error` and `ocr_error` details
  alongside enrichment status.
- Built-in runtime exclusions now skip operating-system, recovery, installer,
  temporary, cache, recycle, and virtual filesystem locations even for
  existing roots.
- Admin sign-in validates credentials through a lightweight session endpoint,
  avoiding SQLite/catalog contention during login and clearly reporting
  connecting, rejected-token, and unavailable-service states.

### Changed

- Windows sidecar setup is self-contained: QIndexer installs and resolves its
  private Tesseract, Ghostscript, Python, and OCRmyPDF runtime without relying
  on pre-existing system utilities.

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

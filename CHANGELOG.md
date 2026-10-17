# Changelog

## 1.2.41 - 2026-10-17

- Populate the advanced JSON configuration editor automatically after a valid
  admin session is established, show its loading state, and give the editor a
  practical working height.
- Make index storage maintenance backend-aware in the admin UI. PostgreSQL
  deployments run and report `VACUUM (ANALYZE)` after text trimming; SQLite
  deployments report their optimize, checkpoint, and vacuum sequence.

## 1.2.40 - 2026-10-17

- Add severity, source, and text filters to the admin log view. The default
  attention filter reduces routine debug noise while retaining errors and
  warnings; live refresh no longer forces a reader back to the bottom.
- Add an advanced JSON configuration editor to the admin UI. It uses the
  portable export/import format, validates changes before save, and preserves
  redacted database credentials.

## 1.2.39 - 2026-10-17

- Batch coalesced filesystem hints per root into durable metadata writes,
  avoiding a database transaction and crawl generation for each individual
  notification.
- Ignore attribute-only and removal notifications. A modified file triggers an
  immediate refresh only when enabled content extraction or OCR can use it;
  ordinary metadata changes wait for normal reconciliation.
- Back off empty content, OCR, and hash claim passes from one second to a
  thirty-second ceiling, lowering idle PostgreSQL activity without delaying
  active queues.
- Verify that content and OCR updates replace existing extracted text rather
  than accumulating duplicate enrichment data.

## 1.2.37 - 2026-10-17

- Introduce explicit enrichment routing: OCR-enabled PDFs and raster images
  bypass native content extraction and enter the OCR queue directly; Office
  and plain-text documents remain on the fast content path.
- Make OCR independent of content extraction, and reconcile previously queued
  PDFs/images to direct OCR at startup and after a configuration change without
  resetting completed work or terminal retry state.
- Identify OneDrive and other inaccessible cloud placeholders as unavailable
  sources in logs instead of incorrectly calling them ineligible content.

## 1.2.36 - 2026-10-17

- Release only orphaned queued content/OCR/hash claims at startup. An unclean
  process stop can no longer leave documents permanently reserved by workers
  that no longer exist; no indexed documents or extracted text are removed.

## 1.2.35 - 2026-10-17

- Fix the content/OCR claim query to normalize dotted admin extensions such as
  `.png` and `.pdf` to the catalog's bare extension form before SQL matching.
  Previously, excluded files could be claimed, released, and immediately
  claimed again, leaving content workers busy without extracting anything.
- Apply the same normalization to actionable-backlog totals so the dashboard
  agrees with the worker claim rules.

## 1.2.34 - 2026-10-16

- Contain third-party content-parser panics to the individual file that caused
  them. A malformed or transiently unreadable PDF is now marked unindexable
  for content instead of silently terminating a content worker.
- Detect a buffered content queue with no waiting or in-flight worker, log the
  exact state, and start a bounded replacement worker to recover without an
  index reset.
- Make content, OCR, and hash backlog totals respect enabled roots, per-root
  overrides, file-size limits, and exclusions. Disabled hashing is no longer
  reported as a pending hash backlog.

## 1.2.33 - 2026-10-16

- Prevent watcher hint traffic from starving content extraction, OCR, or
  hashing: full/reconciliation crawls retain priority, while lightweight
  targeted hints may run alongside enrichment work.
- Coalesce distinct filesystem changes into one per-root hint batch every 15
  seconds by default, substantially reducing unnecessary PostgreSQL activity
  on busy local folders.
- Add content worker, wait-state, and buffered-job metrics for immediate
  visibility into an enrichment backlog that is deferred versus stalled.

## 1.2.32 - 2026-10-16

- Coalesce repeat filesystem hints for the same path for 15 minutes by default,
  preventing a busy application directory from becoming a continuous crawl.
- Treat only genuinely new directories as recursive watcher work; existing
  watched directories and files receive targeted refreshes.
- Defer content extraction, OCR, and hashing until every structural crawl is
  complete, and retain watcher hints while CPU or disk pressure pauses work.
- Show queued background work in the admin operations panel rather than
  presenting it as idle.

## 1.2.31 - 2026-10-16

- Make administrative crawler stop a hard boundary: queued, watcher, scheduled,
  and manual work remains stopped until an explicit resume, while a service
  restart can still resume due work normally.
- Avoid recursive recrawls from ordinary directory-change notifications; new
  files continue to trigger recursive discovery where needed.
- Clarify enrichment diagnostics so unavailable engines, deferred retries, and
  files that are not eligible for OCR or content extraction are distinguishable.
- Hide protected document totals until an administrator has authenticated.

## 1.2.30 - 2026-10-15

- Stop issuing `ANALYZE documents` every five seconds in PostgreSQL mode;
  PostgreSQL now handles routine checkpoint, analyze, and autovacuum work on
  its own cadence.
- Make the explicit Compact Index maintenance action run standard PostgreSQL
  `VACUUM (ANALYZE)`, reclaiming dead tuples for reuse without an exclusive
  `VACUUM FULL` rewrite.

## 1.2.29 - 2026-10-15

- Keep PostgreSQL search-vector input below its hard 1 MiB limit, avoiding
  repeated `SQLSTATE 54000` failures on large generated text assets.
- Treat browser cache, service-worker, web-storage, IndexedDB, and local-storage
  folders beneath AppData as built-in non-indexable locations.
- Apply global path exclusions during traversal and fix nested `**/.../**`
  directory rules to match the directory node itself as well as its descendants.

## 1.2.28 - 2026-10-15

- Sanitize extracted and OCR text before storage, including malformed UTF-8 and
  NUL bytes that PostgreSQL rejects. This prevents repeated content-update
  failures and the corresponding database error spam from malformed files.
- Apply path exclusions to filesystem notification hints before they are
  indexed, keeping protected operating-system directories out of watcher work.
- Suppress expected cancellation diagnostics during a clean stop or restart so
  the operational log stays focused on real crawler failures.

## 1.2.27 - 2026-10-15

- Keep each OCR command override in its own normal-height grid cell, even when
  it shares a row with a multiline exclusion field.

## 1.2.26 - 2026-10-15

- Compact the OCR executable overrides into a single horizontal command row in
  the indexing settings UI.

## 1.2.25 - 2026-10-15

- Unify extension-policy inputs as dotted values such as `.pdf`, `.xml`, and
  `.h`. Root file types plus global/per-root content and OCR exclusions now
  normalize legacy bare entries during load and save them canonically through
  the admin API/UI. File wildcard fields intentionally remain glob patterns
  such as `*.pdf` and `**/Finance/*.pdf`.
- Make the indexing-settings save action visible: it now disables while saving
  and reports `Saving...`, `Saved`, or `Save failed` beside the button.

## 1.2.24 - 2026-10-15

- Fix admin numeric validation for metadata queue and database batch settings.
  Host-sized values such as `4096` are valid integer configuration values and
  no longer fail the browser's incorrect `step=100` constraint.

## 1.2.23 - 2026-10-15

- Apply adaptive-pressure pausing to an initial crawl that is already in
  progress, not only to later reconciliation passes.

## 1.2.22 - 2026-10-15

- Decouple filesystem notification intake from hint crawls. Changed paths now
  coalesce in a per-root queue, so a large subtree or a concurrently running
  reconciliation cannot freeze watcher intake or drop a notification.
- Bound root watch registration and reserve 40% of the configured watcher
  budget for hot folders. Admin metrics now distinguish pinned and hot watches
  and show pending changed paths.
- Add `adaptive_throttle.pressure_samples`, defaulting to three. Sustained
  external CPU or disk pressure pauses traversal and background content/OCR/
  hash refill until the configured healthy-sample recovery threshold is met.

## 1.2.21 - 2026-10-15

- Add per-root content and OCR extension overrides. Roots inherit the global
  exclusion lists by default, or may replace either list, including with an
  explicit empty list.
- Apply global crawler file and folder hygiene exclusions to traversal,
  reconciliation, watcher selection, and root cleanup. Default filters now
  cover common temporary files, shell metadata, recycle trees, snapshots, sync
  metadata, and local development folders without duplicating those rules per
  root.
- Reflow the admin configuration view into responsive two-column operational
  sections, and make inherited versus overridden enrichment exclusions clear in
  each root's Rules panel.

## 1.2.20 - 2026-10-15

- Add global, line-based file-type exclusions for ordinary content extraction
  and OCR. Queue claims and backlog counts now honor the configured lists.
- Exclude common image formats from ordinary text extraction by default and
  route newly indexed eligible images directly to OCR when enabled.
- Restrict OCR candidates to supported PDFs and image formats.
- Make the full reconciliation interval editable in the admin UI, apply it to
  the live scheduler, and use a six-hour default in example configurations.

## 1.2.19 - 2026-10-14

- Upgrade only the exact historical crawler default profile on startup to the
  host-sized performance profile. Administrator-customized crawl settings are
  preserved unchanged.

## 1.2.18 - 2026-10-14

- Remove SQLite's single-writer gate from PostgreSQL metadata batches so
  independent roots and batches can use the configured PostgreSQL connection
  pool concurrently.
- Size new crawler configurations from the host's logical CPU count and expose
  concurrent-root, metadata-queue, and database-batch limits in the admin UI.
- Use a multi-sample root latency probe, preserve conservative limits for
  high-latency storage, and display live per-root indexing rate, queue depth,
  batch count, and logical indexed size.
- Rename the UI's former I/O-rate counter to indexed-size rate. It measures
  indexed file sizes during metadata traversal, not physical disk throughput.

## 1.2.17 - 2026-10-14

- Fix PostgreSQL search binding for field-scoped queries. The query predicate
  and its values are now assembled together, preventing a four-field QSurfer
  search from generating more placeholders than supplied arguments.
- Add a regression test for QSurfer's four-field prefix/relevance request that
  verifies the bound PostgreSQL placeholder count exactly matches the values.

## 1.2.16 - 2026-10-14

- Fix private PostgreSQL DSNs generated by Windows PowerShell 5.1. The
  database-name interpolation is now explicitly delimited before the
  `?sslmode=disable` query string, preventing the malformed `/=disable` path
  that made an otherwise healthy PostgreSQL server reject QIndexer startup.
- Recognize and repair only that malformed loopback private-PostgreSQL DSN
  during package and Windows service setup. Custom or remote PostgreSQL DSNs
  are left untouched.

## 1.2.15 - 2026-10-14

- Detect an existing Windows QIndexer service by the NSSM `Application`
  registration for the installed `qindexer.exe`, rather than assuming the
  service is named `QIndexer`. This preserves QSurfer-managed names such as
  `QSurferQIndexer` during upgrades.
- Defer stopping the discovered service until immediately before replacing the
  executable. An upgrade now stops the actual service, restarts its dedicated
  PostgreSQL dependency in order, and starts the same service name when
  maintenance completes.

## 1.2.14 - 2026-10-14

- Stream the Windows PostgreSQL installer download and publish downloaded
  bytes, total bytes, and transfer rate in the installer progress document.
- Cache the exact upstream PostgreSQL installer under the QIndexer install
  directory and reuse it on subsequent repair attempts. A future bundled
  PostgreSQL version uses a different cache filename and downloads once.

## 1.2.13 - 2026-10-14

- Make Windows maintenance deterministic: wait for QIndexer to stop before
  replacing files, stop its dedicated PostgreSQL service during that window,
  and restore both services in dependency order.
- Make incomplete private PostgreSQL cleanup wait for only its own process
  tree before retrying file removal; unrelated PostgreSQL services are never
  terminated.
- Fix the Windows installer ACL helper for both files and directories under
  stock Windows PowerShell 5.1.

## 1.2.12 - 2026-10-14

- Recover only an incomplete private Windows PostgreSQL setup when a package
  installer still has no usable QIndexer configuration or has the generated
  placeholder DSN. The recovery replaces that isolated bootstrap service; it
  does not run for established QIndexer configurations.

## 1.2.11 - 2026-10-14

- Fix the Windows private PostgreSQL bootstrap on Windows PowerShell 5.1: the
  port probe is now null-safe and fresh role/database existence checks handle
  empty `psql` output correctly.
- Add optional atomic installer progress files for package and service setup on
  Windows and Linux. An updater can poll the JSON file while QIndexer is
  intentionally stopped.

## 1.2.10 - 2026-10-14

- Fix private PostgreSQL credential generation in the packaged Windows setup
  helper on stock Windows PowerShell 5.1.

## 1.2.9 - 2026-10-14

- Make PostgreSQL the default QIndexer catalog, with pooled client access,
  native full-text search, catalog-size metrics, and SQLite retained as an
  explicit portable option.
- Add self-contained PostgreSQL setup helpers to Windows and Linux packages.
  A fresh package install provisions a loopback-only local database and writes
  its generated DSN into the protected QIndexer configuration.
- Redact database DSNs from configuration/export responses and document the
  QSurfer integration contract for PostgreSQL-backed installs.

## 1.2.8 - 2026-10-14

- Keep the authenticated admin shell responsive on large SQLite catalogs by
  serving cached dashboard aggregates while a single low-priority refresh
  samples file types, enrichment backlog, and stored text size.
- Avoid overlapping dashboard analytics scans, add an aggregate-friendly index
  for active file-type counts, and show clear loading states until the first
  snapshot is available.

## 1.2.7 - 2026-10-13

- Bound hot-folder watcher registration so slow, unavailable, or stale
  candidate paths cannot keep filesystem notifications from entering their
  normal event and rebalance loop.
- Skip built-in excluded paths when selecting hot watch candidates.

## 1.2.6 - 2026-10-13

- Reconcile built-in operational exclusions consistently for folders and their
  descendants. Historical entries below excluded paths are removed instead of
  being mislabeled as missing after a successful crawl.
- Preserve files that become temporarily unreadable as active records with an
  `inaccessible` access status, rather than treating an access error as proof
  that the file was deleted.

## 1.2.5 - 2026-10-13

- Keep root maintenance isolated: deleting, clearing, or changing one root no
  longer interrupts content extraction, OCR, or hashing for other roots.
- Cache storage metrics so live admin refreshes do not repeatedly scan all
  indexed text.
- Keep root-rule saves responsive while existing entries are reconciled, and
  reject stale concurrent rule edits rather than overwriting newer
  configuration.
- Make filesystem watcher root probes bounded and self-healing after share
  reconnects; report unavailable watch paths in watcher status.
- Treat filesystem remove and rename notifications as reconciliation hints
  rather than immediately marking entire subtrees missing.
- Make manual crawl retry resets atomic with root crawl admission.

## 1.2.4 - 2026-10-13

- Keep automatic content extraction and OCR failures bounded at three attempts per unchanged file.
- Make an operator-requested root crawl reset only that root's abandoned enrichment retries for a deliberate fresh attempt.

## 1.2.3 - 2026-10-13

- Run passive SQLite WAL checkpoints during active indexing instead of silently skipping every maintenance cycle while a writer holds the catalog mutex.
- Continue to reserve WAL truncation for a quiet write window, keeping crawler and search work responsive.

## 1.2.2 - 2026-10-12

- Start filesystem notifications before the optional hot-folder activity query, so a busy SQLite index cannot leave the watcher falsely reported as unavailable.
- Bound hot-folder lookups during startup and rebalance; a busy catalog defers that enrichment rather than blocking active root-level watches.

## 1.2.1 - 2026-10-12

- Prevent excluded historical paths from being revived by the missing-path priority queue and immediately marked missing again at the end of the same crawl.
- Clean up entries excluded by the current root rules during successful full-crawl reconciliation, including records that were already missing before the rule change.

## 1.2.0 - 2026-10-12

- Fix stale checkpoint reuse that could cause manual and scheduled full crawls to skip completed parent directories, leaving large missing-record sets unreconciled.
- Full crawls now rebuild traversal from the configured root and clear checkpoints only after a clean reconciliation, while interrupted runs still retain checkpoints for diagnostics.

## 1.1.10 - 2026-10-12

- Apply root rules immediately to the existing catalog when rules are saved. Entries excluded by folder, file, extension, or generic path rules are deleted rather than being marked missing during the next crawl.
- Rule cleanup includes historical missing records, batches catalog writes to keep the service responsive, and reports the number of removed entries in the admin UI.

## 1.1.9 - 2026-10-12

- Correct missing-document retention: each successful full crawl now advances already-missing records, and records are physically removed after `missing_after_successful_crawls` clean misses instead of being retained as hidden rows indefinitely.
- Add standalone service setup scripts: Windows uses NSSM and Linux writes a native systemd unit. Both preserve an existing configuration, otherwise create a loopback-only configuration with no roots or token, offer optional OCR sidecar setup, and start the service.

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

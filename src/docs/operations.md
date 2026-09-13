# Operations Guide

## Deployment Model

QIndexer should run on a machine that can see the storage paths directly:

- Windows workstation/server with mapped drives or UNC share access.
- Linux host with CIFS/NFS mounts.
- Test machine connected over VPN/WireGuard.

The service account controls what can be indexed. QIndexer reports access and
ownership metadata when configured, but opening files remains controlled by the
OS, domain, NAS, and share permissions.

## First Run

1. Copy the binary and config.
2. Start QIndexer:

   ```powershell
   .\qindexer_windows_amd64.exe run --config .\configs\example.yaml
   ```

3. Open `http://127.0.0.1:41974/`.
4. Set the first admin token.
5. Add or edit roots.
6. Validate roots.
7. Start a crawl.

Until the first token is set, admin and search API calls return setup-required
errors.

## Default Ports

- Search API: `127.0.0.1:41973`
- Admin API/UI: `127.0.0.1:41974`

For remote testing, add explicit bind addresses:

```yaml
server:
  bind_addresses:
    - "127.0.0.1:41973"
    - "10.8.0.2:41973"
management:
  bind_addresses:
    - "127.0.0.1:41974"
    - "10.8.0.2:41974"
```

Restart after changing network settings.

## NAS And VPN Notes

Use the host OS to mount or map shares before indexing.

Windows UNC:

```yaml
path: "\\\\nas01\\finance"
credential_ref: "windows-service-account"
```

Linux CIFS:

```yaml
path: "/mnt/nas-finance"
credential_ref: "systemd-mount-secret"
```

Mapped drive with aliases:

```yaml
path: "\\\\nas01\\shared"
path_aliases:
  - id: "shared-x"
    platform: "windows-drive"
    path: "X:\\"
```

If the same root moves between machines, keep the root ID and update the
canonical path/aliases. Then use `Repair index`.

## Repair Workflow

Use root repair when:

- A Windows path such as `X:\...` and a Linux mount path both appear in results.
- A service-local path appears wrapped in a machine/UNC prefix.
- A root was moved to another host and should not be re-indexed from scratch.
- An alias target was corrected.

Steps:

1. Open the admin UI.
2. Edit the root rules.
3. Confirm the canonical `Root path`.
4. Add aliases for other client views.
5. Use `Save and repair index`, or save then click `Repair index`.

Repair rewrites catalog rows and checkpoints only. It does not touch source
files. The result shows aliases checked, paths rewritten, and duplicates merged.

## Clearing And Deleting

`Clear index` removes indexed records for one root but keeps the root
configured. Use this when rules changed substantially and a root should be
rebuilt.

`Delete root` removes the root from config and marks its indexed records as
deleted/no-op. Source files are never deleted.

## Shutdown

Use `Stop service` in the UI or send an OS signal. QIndexer cancels active
crawls, waits for completed directory sets/checkpoints, flushes SQLite, and
closes the DB.

## Crawler Scheduling

QIndexer crawls when:

- The service starts and the crawler loop schedules enabled roots.
- `scan_interval_seconds` elapses for reconciliation.
- The watcher records dirty paths and schedules follow-up work.
- An admin manually triggers a root crawl.

Crawler state is visible in the admin UI metrics cards.

## Performance Tuning

Start conservatively on NAS/VPN paths:

```yaml
root_parallelism: 1
directory_worker_count: 4
metadata_worker_count: 8
metadata_queue_size: 5000
index_batch_size: 1000
```

For fast local disks or idle servers, raise:

- `directory_worker_count`
- `metadata_worker_count`
- `metadata_queue_size`

For slow VPN/NAS links, lower directory workers first. Watch:

- Files/sec
- Dirs/sec
- I/O rate
- CPU load
- Disk busy
- NAS latency

Content extraction, OCR, hashing, and ownership collection can be expensive.
Enable them per root and keep worker counts low until the host has headroom.

## Adaptive Throttling

Adaptive throttle pauses crawl scheduling when CPU or disk pressure crosses
configured thresholds. It resumes after `recovery_samples` healthy samples.

Initial indexing can be made more aggressive by raising thresholds or disabling
adaptive throttle temporarily, but long-running shared machines should keep it
enabled.

## Watcher And Reconciliation

Filesystem watchers are opportunistic. They provide speed by noticing changed
directories, but they are not the only correctness mechanism. Periodic full
reconciliation catches missed events, unavailable roots, and watcher overflow.

## Backups

Use:

- `GET /admin/v1/config/export`
- `POST /admin/v1/config/import`

Exports exclude secrets. Back up the SQLite files only when the service is
stopped or after a clean SQLite checkpoint.

## Diagnostics

Use `GET /admin/v1/diagnostics` or the UI diagnostics view. It includes:

- Redacted config.
- Service version and generation.
- Root states.
- Recent crawls.
- Crawler metrics.
- Recent service log lines.

HTTP request logs include method, path, status, duration, bytes, and remote.
Admin mutations log root IDs, counts, paths, and failure reasons.

QIndexer writes an append-only log file at:

```text
<data_dir>/logs/qindexer.log
```

The file log uses debug level and includes source locations. Recovered panics
from HTTP handlers, crawler routines, and extraction failures include stack
traces so a crash-class failure leaves evidence in the UI and on disk.

On startup, any crawl records left in `running` or `hint_running` state are
marked `interrupted`, and the root state records that the prior service stopped
before the crawl finished.

## Troubleshooting

No admin access:

- Open from localhost for first token setup.
- Confirm the bearer token in the UI.
- Check for `401 unauthorized` in logs.

No search results:

- Confirm QSurfer is pointed at the search API port, not the admin port.
- Check `GET /v1/health`.
- Check root status and document count.
- Verify scope aliases and include/exclude paths.

Duplicate paths:

- Confirm the same physical storage is one root ID, not multiple roots.
- Add aliases for alternate client paths.
- Use `Repair index`.
- If paths contain a host prefix before the canonical mount path, repair should
  strip that wrapper and merge duplicates.

Slow NAS crawl:

- Lower directory workers over VPN.
- Raise workers only on idle hosts and fast links.
- Check adaptive throttle state.
- Disable expensive enrichment until metadata indexing is complete.

Large database:

- Content text, OCR text, and hashes increase DB size.
- Clear roots that were accidentally indexed.
- Stop the service cleanly so WAL files checkpoint.

Wrong display path:

- Check root aliases.
- Add `target` when an alias maps to a subfolder under the canonical root.
- Clients should display `display_path || path`.

## Windows Service

Use the installer script from the repository root:

```powershell
.\scripts\install-qindexer.ps1 -InstallDir "C:\Program Files\QIndexer" -ConfigPath .\src\configs\windows-nas.example.yaml -InstallSidecars -InstallService -StartService
```

The binary also supports direct service wrappers:

```powershell
qindexer_windows_amd64.exe run --config C:\ProgramData\QIndexer\config.yaml
```

Run as an account with read access to every root.

## systemd

Use the Linux installer script from the repository root:

```bash
./scripts/install-qindexer-linux.sh --install-dir /opt/qindexer --config ./src/configs/linux-nas.example.yaml --install-sidecars --install-service --start-service
```

`--install-sidecars` attempts the available package manager, then verifies
`tesseract`, `ocrmypdf`, `gs`, and `pdftotext`. If your distro uses different
package names, install those commands manually or set explicit command paths in
`qindexer.yaml`.

Example unit:

```ini
[Unit]
Description=QIndexer
After=network-online.target

[Service]
ExecStart=/usr/local/bin/qindexer run --config /etc/qindexer/config.yaml
Restart=on-failure
User=qindexer
Group=qindexer

[Install]
WantedBy=multi-user.target
```

Mount CIFS/NFS shares before starting the service.

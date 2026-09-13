# QIndexer

QIndexer is a portable filesystem indexer and HTTP search service for QSurfer.
It crawls local disks, mapped drives, UNC shares, Linux mounts, and NAS trees
into a SQLite-backed catalog, then exposes QSurfer-compatible metadata,
folder, content, and autocomplete search.

QIndexer is built for large office storage: bounded worker pools, checkpointed
crawls, one serialized SQLite writer, concurrent search reads, incremental
filesystem hints, periodic reconciliation, repair tools, and adaptive CPU/disk
throttling are included.

QIndexer indexes and reports what the service account can see. It does not
replace operating system, domain, or NAS permissions.

## Highlights

- File and folder indexing with stable result IDs.
- Filename, path, extension, metadata, and optional content search.
- Optional Office, PDF, text, OCR, hash, and owner metadata enrichment.
- Folder-only search and indexed path suggestion for address bars.
- Windows drive letters, UNC paths, Linux mounts, and wildcard roots.
- Root aliases for cross-machine path translation.
- Canonical `path` plus client-friendly `display_path` in search results.
- Segment-aware path scopes so `C:\Legal` does not match `C:\Legalities`.
- Root-scoped include/exclude rules for file types, file wildcards, folder
  wildcards, and legacy path patterns.
- Search pagination, sorting, highlights, matched fields, freshness, and root
  status metadata.
- Local admin web UI for first-run token setup, root/rule management, crawler
  controls, diagnostics, network bindings, repair, index clearing, and
  configurable CPU/disk pressure throttling.
- Multi-interface listening for search and admin APIs.
- Root repair tools that merge duplicate rows after moving between mapped
  drives, UNC paths, and mounted NAS paths.

## Quick Start

Requirements:

- Go 1.24.1 or newer for building from source.
- Windows, Linux, or another Go-supported host with filesystem access to the
  roots being indexed.
- Optional: Tesseract and OCRmyPDF for OCR.

Windows:

```powershell
cd src
.\scripts\build.ps1
.\dist\qindexer_windows_amd64.exe run --config .\configs\example.yaml
```

Linux:

```bash
cd src
./scripts/build.sh
./dist/qindexer_linux_amd64 run --config ./configs/example.yaml
```

Portable install scripts:

```bash
./scripts/install-qindexer-linux.sh --install-dir /opt/qindexer --config ./src/configs/linux-nas.example.yaml --install-sidecars --install-service --start-service
```

```powershell
.\scripts\install-qindexer.ps1 -InstallDir "C:\Program Files\QIndexer" -ConfigPath .\src\configs\windows-nas.example.yaml -InstallSidecars -InstallService -StartService
```

Open the admin UI:

```text
http://127.0.0.1:41974/
```

On a fresh config, QIndexer waits for an admin token to be set from the local
management UI. Once set, the same token secures both search and admin APIs
unless separate token files are configured.

## Command Line

```text
qindexer run --config configs/example.yaml
qindexer crawl --config configs/example.yaml --root root-id
qindexer crawl --config configs/example.yaml
qindexer status --config configs/example.yaml
qindexer version
```

`run` starts the search API, admin API, crawler loop, watcher, and background
content/OCR/hash workers. `crawl` performs a one-shot crawl and exits.

## Documentation

- [Configuration](src/docs/configuration.md): every config field, root rule,
  alias format, and enrichment setting.
- [API](src/docs/api.md): search/admin protocol, request and response shapes,
  errors, pagination, sorting, scopes, repair, and diagnostics.
- [HTTP Client Guide](src/docs/http-client.md): copy-ready requests for any
  client, without a QSurfer dependency.
- [Operations](src/docs/operations.md): deployment, tuning, NAS behavior,
  shutdown, troubleshooting, backup/export/import, and repair workflows.
- [Changelog](CHANGELOG.md): release history.

## Default Ports

| Surface | Default |
| --- | --- |
| Search API | `127.0.0.1:41973` |
| Admin API | `127.0.0.1:41974` |
| Admin UI | `http://127.0.0.1:41974/` |

Both APIs can listen on multiple interfaces:

```yaml
server:
  bind: "127.0.0.1:41973"
  bind_addresses:
    - "127.0.0.1:41973"
    - "10.8.0.2:41973"
management:
  bind: "127.0.0.1:41974"
  bind_addresses:
    - "127.0.0.1:41974"
```

Expose remote admin endpoints only on trusted networks or behind TLS and
network access controls.

## Root Aliases

Each root has one canonical service path. Aliases describe client-visible paths
for the same storage:

```yaml
roots:
  - id: "shared"
    path: "/srv/qindexer/mounts/team-share"
    path_aliases:
      - id: "shared-x"
        platform: "windows-drive"
        path: "X:\\"
      - id: "legal-x"
        platform: "windows-drive"
        path: "X:\\Legal"
        target: "/srv/qindexer/mounts/team-share/Legal"
```

Search responses keep `path` stable and canonical. `display_path` is filled
from the best matching alias so a Windows client can show `X:\...` while still
using canonical paths for identity, caching, and repair.

## Admin UI

The web UI supports:

- First-run admin token bootstrap.
- Locked-state health overview with no root, path, rule, activity, or log data
  until the admin token is accepted.
- Root creation, deletion, validation, manual crawl, clear-index, and repair.
- Per-root path aliases and include/exclude rules.
- Per-root content extraction, OCR, hashing, and ownership toggles.
- Crawler pause/resume and service stop.
- Network binding configuration.
- Live metrics including operations/sec, I/O rate, active crawler state, index
  size, and adaptive CPU/disk throttle state.
- Recent crawl activity, root errors, log tail, and diagnostics.
- File logging at `<data_dir>/logs/qindexer.log` with debug entries and panic
  stack traces.

## Development

```powershell
cd src
go test ./...
go build ./cmd/qindexer
```

The public repository is `https://github.com/senposage/qindexer`.

## License

QIndexer is released under the [MIT License](LICENSE).

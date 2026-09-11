# QIndexer

QIndexer is a portable filesystem indexer and search service for QSurfer. It
crawls local disks, UNC shares, and mounted NAS paths into a SQLite-backed
metadata index, then exposes a small authenticated HTTP API for search, folder
resolution, and autocomplete.

It is built for sprawling file trees without treating a NAS like a stress test:
bounded crawler queues, one serialized SQLite writer, concurrent search reads,
filesystem-watch hints, checkpointed crawls, periodic reconciliation, and
adaptive CPU/disk throttling are all built in.

## What It Does

- Indexes files and folders as first-class results.
- Searches filename, path, extension, and optionally extracted content.
- Crawls Windows drive letters, UNC shares, Linux mounts, and wildcard roots.
- Persists crawl checkpoints and catalog state in SQLite.
- Offers a local web admin UI for roots, rules, crawl controls, and diagnostics.
- Returns QSurfer-ready paging, result IDs, ETags, root status, freshness, and folder-only search/suggest responses.
- Advertises root aliases so `X:\`, `\\server\share`, and `/mnt/share` can be recognized as views of the same indexed root.

QIndexer observes filesystem access; it does not replace OS or NAS permissions.
The service can report best-effort owner and access metadata, but the host and
share remain the authority for opening files.

## Quick Start

Requirements: Go 1.24.1 or newer.

```powershell
cd src
.\scripts\build.ps1
.\dist\qindexer_windows_amd64.exe run --config .\configs\example.yaml
```

```bash
cd src
./scripts/build.sh
./dist/qindexer_linux_amd64 run --config ./configs/example.yaml
```

Open the admin UI at `http://127.0.0.1:41974/`. On a fresh configuration, set
an admin token there before either API accepts requests. The initial token
secures both search and administration by default.

## Endpoints

| Endpoint | Purpose |
| --- | --- |
| `GET /v1/health` | Service, protocol, and index health |
| `GET /v1/capabilities` | Supported filters, sorts, limits, and features |
| `GET /v1/roots` | Indexed roots, canonical paths, aliases, and crawl state |
| `POST /v1/search` | Paged metadata/content search |
| `POST /v1/directories` | Indexed folder-only lookup |
| `POST /v1/suggest` | Indexed folder/path autocomplete |
| `http://127.0.0.1:41974/` | Local management UI |

The protocol is documented in [src/docs/api.md](src/docs/api.md). QSurfer
integration notes and the shared provider contract live in
[AGENT-WATCH.md](AGENT-WATCH.md) and
[src/contracts/qsurfer-search-v1.json](src/contracts/qsurfer-search-v1.json).

## Roots And NAS Paths

Roots are configured independently, each with its own include/exclude rules.
Examples are in [src/configs](src/configs), including Windows UNC and Linux NAS
setups. A root may advertise client-facing aliases:

```yaml
path: "\\\\nas01\\finance"
path_aliases:
  - id: finance-x-drive
    platform: windows-drive
    path: "X:\\Finance"
  - id: finance-linux-mount
    platform: linux
    path: "/mnt/finance"
```

Aliases translate an explicit folder scope only. Raw search terms are never
rewritten.

## Performance And Optional Work

The crawler defaults to bounded concurrency and pauses itself under configured
CPU or disk pressure. Filesystem watches supply incremental hints where the OS
supports them; scheduled full crawls reconcile anything missed.

Office/PDF/plain-text extraction and SHA-256 hashing run in separate bounded
background queues and are disabled by default. Enable them only after deciding
the appropriate I/O budget for the host and NAS.

## Development

```powershell
cd src
go test ./...
```

Operational deployment notes are in [src/docs/operations.md](src/docs/operations.md).

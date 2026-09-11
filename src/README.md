# QIndexer

Standalone metadata/path search service for QSurfer.

Phase 1 goals:

- Flat Windows and Linux binaries.
- No Docker runtime requirement.
- Authenticated HTTP search API.
- Local, UNC, and mounted NAS root crawling.
- SQLite catalog plus SQLite FTS search.
- Bounded crawler concurrency and batched writes.
- Service-friendly foreground mode for Windows Service or systemd wrappers.

## Quick Start

```powershell
qindexer.exe run --config configs/example.yaml
```

```bash
./qindexer run --config configs/example.yaml
```

Search API:

```http
POST /v1/search
Authorization: Bearer <search-token>
Content-Type: application/json
```

Admin API:

```http
POST /admin/v1/roots/{root_id}/crawl
Authorization: Bearer <admin-token>
```

Web UI:

```text
http://127.0.0.1:41974/
```

On a fresh configuration, open the local UI and set a token before the search and management APIs are enabled. The initial token secures both APIs; the UI stores it in browser session storage only, and the service stores it in the YAML file with restrictive permissions.

## Build

Install Go 1.24.1 or newer, then:

```powershell
.\scripts\build.ps1
```

or:

```bash
./scripts/build.sh
```

Artifacts are written to `dist/`.

## Example Configs

- `configs/example.yaml`: local development.
- `configs/windows-nas.example.yaml`: Windows Service with UNC NAS paths.
- `configs/linux-nas.example.yaml`: systemd host with mounted NAS paths.

Root paths may also use filesystem wildcards, including `X:\*`, `\\nas01\share\*`, and `/mnt/nas-*/*`.

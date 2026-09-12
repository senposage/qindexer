# QIndexer Source

This directory contains the QIndexer Go service.

## Build

```powershell
.\scripts\build.ps1
```

```bash
./scripts/build.sh
```

Manual build:

```powershell
go test ./...
go build -o .\dist\qindexer_windows_amd64.exe .\cmd\qindexer
```

## Run

```powershell
.\dist\qindexer_windows_amd64.exe run --config .\configs\example.yaml
```

```bash
./dist/qindexer_linux_amd64 run --config ./configs/example.yaml
```

Admin UI:

```text
http://127.0.0.1:41974/
```

Search API:

```text
http://127.0.0.1:41973/v1
```

## Layout

- `cmd/qindexer`: CLI entry point.
- `internal/api`: search/admin HTTP API.
- `internal/catalog`: SQLite catalog and FTS search.
- `internal/config`: YAML config model and validation.
- `internal/crawler`: root crawling, rules, throttling, and background work.
- `internal/extract`: content/OCR/hash extraction helpers.
- `internal/service`: service lifecycle.
- `internal/web`: embedded admin UI.
- `configs`: sample configs.
- `docs`: API, configuration, and operations docs.

See the top-level README for the full 1.0 overview.

# QIndexer Portable Test Bundle

This folder contains a portable QIndexer build with the current config and index database.

## Windows

Run from the bundle root:

```powershell
.\bin\qindexer_windows_amd64.exe run --config .\configs\example.yaml
```

Search API:

```text
http://127.0.0.1:41973
```

Admin UI:

```text
http://127.0.0.1:41974
```

## Linux

Run from the bundle root:

```bash
chmod +x ./bin/qindexer_linux_amd64
./bin/qindexer_linux_amd64 run --config ./configs/example.yaml
```

The copied config keeps the existing indexed roots and relative data path. If the
test machine maps the NAS differently, update the root paths or aliases in the
admin UI before crawling.


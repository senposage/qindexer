# Operations Notes

## Default Bindings

- Search API: `127.0.0.1:41973`
- Admin API: `127.0.0.1:41974`
- Admin web UI: `http://127.0.0.1:41974/`

Remote bindings should be explicit in config and should normally sit behind TLS.

## NAS Crawling

Use the host OS to make NAS paths visible.

Windows:

```yaml
path: "\\\\nas01\\finance"
credential_ref: "os-service-account"
```

Linux:

```yaml
path: "/mnt/nas-finance"
credential_ref: "systemd-user-or-mount-secret"
```

If a root is unreachable, the service records the failure and keeps existing active results. Missing/deleted transitions only happen after a successful crawl of that root.

## Performance Defaults

The defaults are deliberately conservative:

- One root crawled at a time.
- Bounded metadata worker pool.
- Bounded metadata queue.
- One SQLite writer path.
- Content extraction disabled in Phase 1.

Raise worker counts per environment after observing NAS latency and service host load.

## Windows Service

Install:

```powershell
.\deploy\install-windows-service.ps1 `
  -BinaryPath "C:\Program Files\QSurferSearch\qindexer.exe" `
  -ConfigPath "C:\ProgramData\QSurferSearch\config.yaml"
```

Start:

```powershell
Start-Service QSurferSearchService
```

## systemd

Copy files:

```bash
sudo cp qindexer /usr/local/bin/
sudo cp deploy/qsurfer-search.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now qsurfer-search.service
```

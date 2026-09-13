package service

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"qindexer/internal/api"
	"qindexer/internal/catalog"
	"qindexer/internal/config"
	"qindexer/internal/crawler"
	"qindexer/internal/watcher"
)

type App struct {
	ConfigPath string
	Logger     *slog.Logger
}

func (a App) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return err
	}
	log := a.Logger
	dataDir := config.ResolveDataDir(a.ConfigPath, cfg.Index.DataDir)
	logFile, logPath, logErr := openLogFile(dataDir)
	if log == nil {
		log = serviceLogger(logFile)
	}
	if logFile != nil {
		defer logFile.Close()
	}
	if logErr != nil {
		log.Warn("file logging unavailable", "path", logPath, "error", logErr)
	} else {
		log.Info("file logging enabled", "path", logPath)
	}
	accessLogFile, accessLogPath, accessLogErr := openNamedLogFile(dataDir, "qindexer-access.log")
	if accessLogFile != nil {
		defer accessLogFile.Close()
	}
	if accessLogErr != nil {
		log.Warn("access logging unavailable; using operational log", "path", accessLogPath, "error", accessLogErr)
	}
	cat, err := catalog.Open(ctx, dataDir)
	if err != nil {
		return err
	}
	defer cat.Close()
	if interrupted, err := cat.MarkOpenCrawlsInterrupted(ctx); err != nil {
		log.Warn("stale crawl recovery failed", "error", err)
	} else if interrupted > 0 {
		log.Warn("stale crawl records marked interrupted", "count", interrupted)
	}
	for _, root := range cfg.Roots {
		if !root.Enabled {
			continue
		}
		if removed, err := cat.PruneRecoveryPaths(ctx, root.ID); err != nil {
			log.Warn("recovery path cleanup failed", "root", root.ID, "error", err)
		} else if removed > 0 {
			log.Info("recovery paths removed from index", "root", root.ID, "documents", removed)
		}
	}
	cr := crawler.New(cfg, cat, log)
	baseDir := filepath.Dir(a.ConfigPath)
	searchToken, err := cfg.Server.Auth.ResolveToken(baseDir)
	if err != nil {
		return err
	}
	adminToken, err := cfg.Management.Auth.ResolveToken(baseDir)
	if err != nil {
		return err
	}
	watch := watcher.New(cfg, cr, log, dataDir)
	apiServer := api.New(cfg, a.ConfigPath, cat, cr, log, searchToken, adminToken)
	apiServer.SetConfigChanged(watch.ApplyConfig)
	apiServer.SetLogPath(logPath)
	if accessLogFile != nil {
		apiServer.SetAccessLogger(slog.New(slog.NewTextHandler(accessLogFile, &slog.HandlerOptions{AddSource: true, Level: slog.LevelDebug})))
		log.Info("http access logging enabled", "path", accessLogPath)
	}
	apiServer.SetShutdown(cancel)

	errs := make(chan error, len(cfg.Server.BindAddresses)+len(cfg.Management.BindAddresses)+2)
	servers := startHTTPServers("search api", cfg.Server.BindAddresses, apiServer.SearchHandler(), log, errs)
	if cfg.Management.Enabled {
		servers = append(servers, startHTTPServers("admin api", cfg.Management.BindAddresses, apiServer.AdminHandler(), log, errs)...)
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer recoverWorker(log, "crawler loop", errs)
		cr.Loop(runCtx)
	}()
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer recoverWorker(log, "filesystem watcher", errs)
		if err := watch.Run(runCtx); err != nil {
			errs <- err
		}
	}()

	var runErr error
	select {
	case <-runCtx.Done():
	case err := <-errs:
		if err != nil {
			runErr = err
		}
	}
	cancel()
	cr.CancelAll()
	for _, server := range servers {
		serverCtx, stopServer := context.WithTimeout(context.Background(), 3*time.Second)
		err := server.Shutdown(serverCtx)
		stopServer()
		if err != nil {
			log.Warn("http server did not drain during shutdown; closing it", "error", err)
			_ = server.Close()
		}
	}
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		log.Warn("service shutdown continuing with blocked background work")
	}
	return runErr
}

func openLogFile(dataDir string) (*os.File, string, error) {
	return openNamedLogFile(dataDir, "qindexer.log")
}

func openNamedLogFile(dataDir, filename string) (*os.File, string, error) {
	logPath := filepath.Join(dataDir, "logs", filename)
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, logPath, err
	}
	if err := os.Chmod(filepath.Dir(logPath), 0o700); err != nil {
		return nil, logPath, err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, logPath, err
	}
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return nil, logPath, err
	}
	return f, logPath, nil
}

func serviceLogger(logFile *os.File) *slog.Logger {
	var out io.Writer = os.Stdout
	if logFile != nil {
		out = io.MultiWriter(os.Stdout, logFile)
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{AddSource: true, Level: slog.LevelDebug}))
}

func recoverWorker(log *slog.Logger, name string, errs chan<- error) {
	if value := recover(); value != nil {
		err := errors.New("worker panic: " + name)
		log.Error("worker panic recovered", "worker", name, "panic", value, "stack", string(debug.Stack()))
		errs <- err
	}
}

func startHTTPServers(name string, addresses []string, handler http.Handler, log *slog.Logger, errs chan<- error) []*http.Server {
	servers := make([]*http.Server, 0, len(addresses))
	for _, addr := range addresses {
		server := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 90 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10}
		servers = append(servers, server)
		go func(addr string, server *http.Server) {
			log.Info(name+" listening", "addr", addr)
			errs <- ignoreClosed(server.ListenAndServe())
		}(addr, server)
	}
	return servers
}

func (a App) CrawlOnce(ctx context.Context, rootID string) error {
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return err
	}
	log := a.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	cat, err := catalog.Open(ctx, config.ResolveDataDir(a.ConfigPath, cfg.Index.DataDir))
	if err != nil {
		return err
	}
	defer cat.Close()
	cr := crawler.New(cfg, cat, log)
	if rootID == "" {
		cr.CrawlAll(ctx)
		return nil
	}
	_, err = cr.CrawlRoot(ctx, rootID)
	return err
}

func (a App) Status(ctx context.Context) error {
	cfg, err := config.Load(a.ConfigPath)
	if err != nil {
		return err
	}
	cat, err := catalog.Open(ctx, config.ResolveDataDir(a.ConfigPath, cfg.Index.DataDir))
	if err != nil {
		return err
	}
	defer cat.Close()
	states, err := cat.RootStates(ctx)
	if err != nil {
		return err
	}
	enc := slog.New(slog.NewTextHandler(os.Stdout, nil))
	for _, root := range cfg.Roots {
		state := states[root.ID]
		status := state.LastCrawlStatus
		if status == "" {
			status = "never"
		}
		enc.Info("root", "id", root.ID, "enabled", root.Enabled, "status", status, "documents", state.DocumentCount, "missing", state.MissingCount)
	}
	return nil
}

func SignalContext() context.Context {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx
}

func ignoreClosed(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

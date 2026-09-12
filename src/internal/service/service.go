package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	dataDir := config.ResolveDataDir(a.ConfigPath, cfg.Index.DataDir)
	cat, err := catalog.Open(ctx, dataDir)
	if err != nil {
		return err
	}
	defer cat.Close()
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
	apiServer := api.New(cfg, a.ConfigPath, cat, cr, log, searchToken, adminToken)
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
		cr.Loop(runCtx)
		errs <- nil
	}()
	workers.Add(1)
	go func() {
		defer workers.Done()
		watch := watcher.New(cfg, cr, log, dataDir)
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, server := range servers {
		_ = server.Shutdown(shutdownCtx)
	}
	cr.CancelAll()
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-shutdownCtx.Done():
		log.Warn("service shutdown timed out waiting for crawler or watcher")
	}
	return runErr
}

func startHTTPServers(name string, addresses []string, handler http.Handler, log *slog.Logger, errs chan<- error) []*http.Server {
	servers := make([]*http.Server, 0, len(addresses))
	for _, addr := range addresses {
		server := &http.Server{Addr: addr, Handler: handler}
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

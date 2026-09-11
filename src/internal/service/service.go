package service

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	cat, err := catalog.Open(ctx, config.ResolveDataDir(a.ConfigPath, cfg.Index.DataDir))
	if err != nil {
		return err
	}
	defer cat.Close()
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
	searchHTTP := &http.Server{Addr: cfg.Server.Bind, Handler: apiServer.SearchHandler()}
	adminHTTP := &http.Server{Addr: cfg.Management.Bind, Handler: apiServer.AdminHandler()}

	errs := make(chan error, 4)
	go func() {
		log.Info("search api listening", "addr", cfg.Server.Bind)
		errs <- ignoreClosed(searchHTTP.ListenAndServe())
	}()
	if cfg.Management.Enabled {
		go func() {
			log.Info("admin api listening", "addr", cfg.Management.Bind)
			errs <- ignoreClosed(adminHTTP.ListenAndServe())
		}()
	}
	go func() {
		cr.Loop(runCtx)
		errs <- nil
	}()
	go func() {
		watch := watcher.New(cfg, cr, log)
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
	_ = searchHTTP.Shutdown(shutdownCtx)
	_ = adminHTTP.Shutdown(shutdownCtx)
	return runErr
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

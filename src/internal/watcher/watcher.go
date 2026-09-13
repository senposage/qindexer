package watcher

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"qindexer/internal/config"
	"qindexer/internal/crawler"
)

type Watcher struct {
	cfg     *config.Config
	cfgMu   sync.RWMutex
	crawler *crawler.Crawler
	log     *slog.Logger
	ignore  string
	reload  chan struct{}
}

type dirtyPath struct {
	path    string
	removed bool
}

func New(cfg *config.Config, cr *crawler.Crawler, log *slog.Logger, ignorePath string) *Watcher {
	return &Watcher{cfg: config.Clone(cfg), crawler: cr, log: log, ignore: filepath.Clean(ignorePath), reload: make(chan struct{}, 1)}
}

func (w *Watcher) ApplyConfig(cfg *config.Config) {
	w.cfgMu.Lock()
	w.cfg = config.Clone(cfg)
	w.cfgMu.Unlock()
	select {
	case w.reload <- struct{}{}:
	default:
	}
}

func (w *Watcher) snapshot() *config.Config {
	w.cfgMu.RLock()
	defer w.cfgMu.RUnlock()
	return w.cfg
}

func (w *Watcher) Run(ctx context.Context) error {
	for {
		reload, err := w.run(ctx)
		if err != nil || ctx.Err() != nil || !reload {
			return err
		}
		w.log.Info("filesystem watcher reloading configuration")
	}
}

func (w *Watcher) run(ctx context.Context) (bool, error) {
	cfg := w.snapshot()
	if !cfg.Watcher.Enabled {
		w.log.Info("filesystem watcher disabled")
		select {
		case <-ctx.Done():
			return false, nil
		case <-w.reload:
			return true, nil
		}
	}
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		w.log.Warn("filesystem watcher unavailable", "error", err)
		return false, nil
	}
	defer fsw.Close()

	rootsByPath := map[string]string{}
	watched := 0
	for _, root := range cfg.Roots {
		if !root.Enabled {
			continue
		}
		paths, err := crawler.ExpandRootPaths(root.Path)
		if err != nil {
			w.log.Warn("watch root unavailable", "root", root.ID, "path", root.Path, "error", err)
			continue
		}
		for _, path := range paths {
			n, err := w.watchTree(ctx, fsw, root, path, rootsByPath, cfg.Watcher.MaxWatchedDirectories-watched)
			watched += n
			if err != nil {
				w.log.Warn("watch tree incomplete", "root", root.ID, "path", path, "watched", watched, "error", err)
			}
			if watched >= cfg.Watcher.MaxWatchedDirectories {
				w.log.Warn("watch directory limit reached", "limit", cfg.Watcher.MaxWatchedDirectories)
				break
			}
		}
	}
	w.log.Info("filesystem watcher started", "directories", watched)

	dirty := map[string]map[string]dirtyPath{}
	var mu sync.Mutex
	var timer *time.Timer
	resetTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(cfg.Watcher.Debounce())
	}

	for {
		var timerC <-chan time.Time
		if timer != nil {
			timerC = timer.C
		}
		select {
		case <-ctx.Done():
			return false, nil
		case <-w.reload:
			return true, nil
		case err, ok := <-fsw.Errors:
			if !ok {
				return false, nil
			}
			if err != nil {
				w.log.Warn("filesystem watcher error", "error", err)
			}
		case event, ok := <-fsw.Events:
			if !ok {
				return false, nil
			}
			if w.shouldIgnore(event.Name) {
				continue
			}
			rootID := rootForEvent(event.Name, rootsByPath)
			if rootID == "" {
				continue
			}
			removed := event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename)
			if event.Has(fsnotify.Create) {
				_ = w.tryWatchCreatedDirectory(fsw, event.Name, rootID, rootsByPath)
			}
			mu.Lock()
			if dirty[rootID] == nil {
				dirty[rootID] = map[string]dirtyPath{}
			}
			dirty[rootID][event.Name] = dirtyPath{path: event.Name, removed: removed}
			mu.Unlock()
			resetTimer()
		case <-timerC:
			mu.Lock()
			batch := dirty
			dirty = map[string]map[string]dirtyPath{}
			timer = nil
			mu.Unlock()
			w.flush(ctx, batch, cfg)
		}
	}
}

func (w *Watcher) watchTree(ctx context.Context, fsw *fsnotify.Watcher, root config.RootConfig, path string, rootsByPath map[string]string, remaining int) (int, error) {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return 0, err
	}
	count := 0
	err = filepath.WalkDir(path, func(p string, entry fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if w.shouldIgnore(p) {
			return filepath.SkipDir
		}
		if remaining > 0 && count >= remaining {
			return filepath.SkipDir
		}
		if err := fsw.Add(p); err != nil {
			return nil
		}
		rootsByPath[filepath.Clean(p)] = root.ID
		count++
		return nil
	})
	return count, err
}

func (w *Watcher) shouldIgnore(path string) bool {
	if w.ignore == "" || w.ignore == "." {
		return false
	}
	rel, err := filepath.Rel(w.ignore, filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (w *Watcher) tryWatchCreatedDirectory(fsw *fsnotify.Watcher, path string, rootID string, rootsByPath map[string]string) error {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return err
	}
	if err := fsw.Add(path); err != nil {
		return err
	}
	rootsByPath[filepath.Clean(path)] = rootID
	return nil
}

func (w *Watcher) flush(ctx context.Context, batch map[string]map[string]dirtyPath, cfg *config.Config) {
	limit := cfg.Watcher.MaxDirtyPathsPerFlush
	for rootID, paths := range batch {
		count := 0
		for _, dirty := range paths {
			if limit > 0 && count >= limit {
				w.log.Warn("dirty path limit reached; falling back to root crawl", "root", rootID, "limit", limit)
				_, _ = w.crawler.CrawlRoot(ctx, rootID)
				break
			}
			_, err := w.crawler.CrawlHint(ctx, rootID, dirty.path, dirty.removed)
			if err != nil {
				w.log.Warn("hint crawl failed", "root", rootID, "path", dirty.path, "error", err)
			}
			count++
		}
	}
}

func rootForEvent(path string, rootsByPath map[string]string) string {
	cleaned := filepath.Clean(path)
	for {
		if rootID := rootsByPath[cleaned]; rootID != "" {
			return rootID
		}
		next := filepath.Dir(cleaned)
		if next == cleaned {
			return ""
		}
		cleaned = next
	}
}

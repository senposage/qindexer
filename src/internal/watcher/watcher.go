package watcher

import (
	"context"
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
	statsMu sync.RWMutex
	stats   Stats
}

type Stats struct {
	Enabled            bool  `json:"enabled"`
	Running            bool  `json:"running"`
	WatchedDirectories int   `json:"watched_directories"`
	MaxDirectories     int   `json:"max_watched_directories"`
	LastEventUnix      int64 `json:"last_event_unix"`
	LastRebalanceUnix  int64 `json:"last_rebalance_unix"`
}

type watchSet struct {
	fsw    *fsnotify.Watcher
	paths  map[string]string
	pinned map[string]bool
	limit  int
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

func (w *Watcher) Stats() Stats {
	w.statsMu.RLock()
	defer w.statsMu.RUnlock()
	return w.stats
}

func (w *Watcher) setStats(stats Stats) {
	w.statsMu.Lock()
	w.stats = stats
	w.statsMu.Unlock()
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
		w.setStats(Stats{Enabled: false})
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

	set := &watchSet{fsw: fsw, paths: map[string]string{}, pinned: map[string]bool{}, limit: cfg.Watcher.MaxWatchedDirectories}
	w.seedWatches(ctx, set, cfg)
	w.updateStats(set, cfg)
	w.log.Info("filesystem watcher started", "directories", len(set.paths), "limit", set.limit, "strategy", "hot_folders")

	dirty := map[string]map[string]dirtyPath{}
	var mu sync.Mutex
	var timer *time.Timer
	resetTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.NewTimer(cfg.Watcher.Debounce())
	}
	rebalance := time.NewTicker(cfg.Watcher.RebalanceInterval())
	defer rebalance.Stop()

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
			rootID := rootForEvent(event.Name, set.paths)
			if rootID == "" {
				continue
			}
			removed := event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename)
			if event.Has(fsnotify.Create) {
				_ = w.tryWatchCreatedDirectory(set, event.Name, rootID)
			}
			w.recordEvent()
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
		case <-rebalance.C:
			w.rebalance(ctx, set, cfg)
			w.updateStats(set, cfg)
		}
	}
}

func (w *Watcher) seedWatches(ctx context.Context, set *watchSet, cfg *config.Config) {
	topLevel := map[string][]string{}
	rootIDs := make([]string, 0)
	for _, root := range cfg.Roots {
		if !root.Enabled {
			continue
		}
		rootIDs = append(rootIDs, root.ID)
		paths, err := crawler.ExpandRootPaths(root.Path)
		if err != nil {
			w.log.Warn("watch root unavailable", "root", root.ID, "path", root.Path, "error", err)
			continue
		}
		for _, path := range paths {
			if !set.add(path, root.ID, true) {
				continue
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() {
					candidate := filepath.Join(path, entry.Name())
					if !w.shouldIgnore(candidate) {
						topLevel[root.ID] = append(topLevel[root.ID], candidate)
					}
				}
			}
		}
	}
	w.addRoundRobin(set, topLevel, rootIDs, true)
	if len(set.paths) >= set.limit {
		return
	}
	hot, err := w.crawler.HotFolders(ctx, rootIDs, cfg.Watcher.ActivityHalfLife(), set.limit-len(set.paths))
	if err != nil {
		w.log.Debug("hot folder lookup failed", "error", err)
		return
	}
	candidates := map[string][]string{}
	for _, item := range hot {
		candidates[item.RootID] = append(candidates[item.RootID], item.Path)
	}
	w.addRoundRobin(set, candidates, rootIDs, false)
}

func (w *Watcher) rebalance(ctx context.Context, set *watchSet, cfg *config.Config) {
	for path := range set.paths {
		if set.pinned[path] {
			continue
		}
		_ = set.fsw.Remove(path)
		delete(set.paths, path)
	}
	rootIDs := enabledRootIDs(cfg)
	if len(set.paths) < set.limit {
		hot, err := w.crawler.HotFolders(ctx, rootIDs, cfg.Watcher.ActivityHalfLife(), set.limit-len(set.paths))
		if err != nil {
			w.log.Debug("hot folder lookup failed", "error", err)
		} else {
			candidates := map[string][]string{}
			for _, item := range hot {
				candidates[item.RootID] = append(candidates[item.RootID], item.Path)
			}
			w.addRoundRobin(set, candidates, rootIDs, false)
		}
	}
	w.statsMu.Lock()
	w.stats.LastRebalanceUnix = time.Now().Unix()
	w.statsMu.Unlock()
	w.log.Debug("filesystem watcher rebalanced", "directories", len(set.paths), "limit", set.limit)
}

func (w *Watcher) addRoundRobin(set *watchSet, candidates map[string][]string, rootIDs []string, pinned bool) {
	for {
		added := false
		for _, rootID := range rootIDs {
			paths := candidates[rootID]
			if len(paths) == 0 {
				continue
			}
			candidates[rootID] = paths[1:]
			if set.add(paths[0], rootID, pinned) {
				added = true
			}
			if len(set.paths) >= set.limit {
				return
			}
		}
		if !added && allCandidatesEmpty(candidates) {
			return
		}
		if allCandidatesEmpty(candidates) {
			return
		}
	}
}

func (s *watchSet) add(path, rootID string, pinned bool) bool {
	path = filepath.Clean(path)
	if s.paths[path] != "" {
		if pinned {
			s.pinned[path] = true
		}
		return false
	}
	if len(s.paths) >= s.limit {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	if err := s.fsw.Add(path); err != nil {
		return false
	}
	s.paths[path] = rootID
	if pinned {
		s.pinned[path] = true
	}
	return true
}

func enabledRootIDs(cfg *config.Config) []string {
	ids := make([]string, 0, len(cfg.Roots))
	for _, root := range cfg.Roots {
		if root.Enabled {
			ids = append(ids, root.ID)
		}
	}
	return ids
}

func allCandidatesEmpty(candidates map[string][]string) bool {
	for _, paths := range candidates {
		if len(paths) > 0 {
			return false
		}
	}
	return true
}

func (w *Watcher) updateStats(set *watchSet, cfg *config.Config) {
	w.statsMu.Lock()
	lastEvent, lastRebalance := w.stats.LastEventUnix, w.stats.LastRebalanceUnix
	w.stats = Stats{Enabled: cfg.Watcher.Enabled, Running: true, WatchedDirectories: len(set.paths), MaxDirectories: set.limit, LastEventUnix: lastEvent, LastRebalanceUnix: lastRebalance}
	w.statsMu.Unlock()
}

func (w *Watcher) recordEvent() {
	w.statsMu.Lock()
	w.stats.LastEventUnix = time.Now().Unix()
	w.statsMu.Unlock()
}

func (w *Watcher) shouldIgnore(path string) bool {
	if w.ignore == "" || w.ignore == "." {
		return false
	}
	rel, err := filepath.Rel(w.ignore, filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (w *Watcher) tryWatchCreatedDirectory(set *watchSet, path string, rootID string) error {
	if w.shouldIgnore(path) {
		return nil
	}
	set.add(path, rootID, false)
	return nil
}

func (w *Watcher) flush(ctx context.Context, batch map[string]map[string]dirtyPath, cfg *config.Config) {
	limit := cfg.Watcher.MaxDirtyPathsPerFlush
	for rootID, paths := range batch {
		activity := make([]string, 0, len(paths))
		for _, dirty := range paths {
			activity = append(activity, dirty.path)
		}
		w.crawler.RecordFolderActivity(ctx, rootID, activity, 5)
		if limit > 0 && len(paths) > limit {
			target := commonDirtyParent(paths)
			if target != "" {
				w.log.Warn("dirty path limit reached; crawling affected scope", "root", rootID, "paths", len(paths), "limit", limit, "path", target)
				if _, err := w.crawler.CrawlHint(ctx, rootID, target, false); err != nil {
					w.log.Warn("affected scope crawl failed", "root", rootID, "path", target, "error", err)
				}
				continue
			}
			w.log.Warn("dirty path limit reached; falling back to root crawl", "root", rootID, "limit", limit)
			_, _ = w.crawler.CrawlRoot(ctx, rootID)
			continue
		}
		count := 0
		for _, dirty := range paths {
			_, err := w.crawler.CrawlHint(ctx, rootID, dirty.path, dirty.removed)
			if err != nil {
				w.log.Warn("hint crawl failed", "root", rootID, "path", dirty.path, "error", err)
			}
			count++
		}
	}
}

func commonDirtyParent(paths map[string]dirtyPath) string {
	common := ""
	for _, item := range paths {
		parent := filepath.Dir(item.path)
		if common == "" {
			common = parent
			continue
		}
		for common != "." && common != string(filepath.Separator) && !isParentPath(common, parent) {
			next := filepath.Dir(common)
			if next == common {
				break
			}
			common = next
		}
	}
	if common == "." || common == string(filepath.Separator) {
		return ""
	}
	return common
}

func isParentPath(parent, path string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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

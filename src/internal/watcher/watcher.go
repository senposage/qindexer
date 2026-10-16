package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	hints   *hintQueue
	statsMu sync.RWMutex
	stats   Stats
}

type Stats struct {
	Enabled            bool   `json:"enabled"`
	Running            bool   `json:"running"`
	Error              string `json:"error,omitempty"`
	WatchedDirectories int    `json:"watched_directories"`
	MaxDirectories     int    `json:"max_watched_directories"`
	PinnedDirectories  int    `json:"pinned_directories"`
	HotDirectories     int    `json:"hot_directories"`
	PendingHintRoots   int    `json:"pending_hint_roots"`
	PendingHintPaths   int    `json:"pending_hint_paths"`
	LastEventUnix      int64  `json:"last_event_unix"`
	LastRebalanceUnix  int64  `json:"last_rebalance_unix"`
}

type watchSet struct {
	fsw         *fsnotify.Watcher
	paths       map[string]string
	pinned      map[string]bool
	limit       int
	pinnedLimit int
	failures    int
	lastError   string
}

type dirtyPath struct {
	path    string
	removed bool
}

const hotFolderLookupTimeout = 2 * time.Second
const hotWatchAddTimeout = 2 * time.Second
const watchRootLookupTimeout = 5 * time.Second
const watchSeedAddTimeout = 5 * time.Second

func New(cfg *config.Config, cr *crawler.Crawler, log *slog.Logger, ignorePath string) *Watcher {
	w := &Watcher{cfg: config.Clone(cfg), crawler: cr, log: log, ignore: filepath.Clean(ignorePath), reload: make(chan struct{}, 1)}
	w.hints = newHintQueue(w)
	return w
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
		if err != nil || ctx.Err() != nil {
			return err
		}
		if reload {
			w.log.Info("filesystem watcher reloading configuration")
			continue
		}
		// Some platforms can temporarily refuse a native notification handle
		// (for example after a network or inotify resource interruption). Keep
		// crawling normally and retry the optional watcher instead of leaving it
		// silently inactive for the rest of the process lifetime.
		timer := time.NewTimer(30 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-w.reload:
			timer.Stop()
			w.log.Info("filesystem watcher reloading configuration")
		case <-timer.C:
			w.log.Info("filesystem watcher retrying after unavailable state")
		}
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
		w.setUnavailable(cfg, err)
		w.log.Warn("filesystem watcher unavailable", "error", err)
		return false, nil
	}
	defer fsw.Close()

	rootCount := len(enabledRootIDs(cfg))
	set := &watchSet{
		fsw: fsw, paths: map[string]string{}, pinned: map[string]bool{}, limit: cfg.Watcher.MaxWatchedDirectories,
		pinnedLimit: pinnedWatchLimit(cfg.Watcher.MaxWatchedDirectories, rootCount),
	}
	w.seedWatches(ctx, set, cfg)
	w.updateStats(set, cfg)
	w.log.Info("filesystem watcher seeded", "directories", len(set.paths), "pinned", len(set.pinned), "pinned_limit", set.pinnedLimit, "limit", set.limit)
	// The native watcher is useful immediately. Do not make its availability
	// depend on an activity query waiting behind a busy index writer.
	w.addHotWatches(ctx, set, cfg)
	w.updateStats(set, cfg)
	w.log.Info("filesystem watcher started", "directories", len(set.paths), "pinned", len(set.pinned), "hot", len(set.paths)-len(set.pinned), "limit", set.limit, "strategy", "hot_folders")

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
				w.setUnavailable(cfg, errors.New("filesystem watcher error stream closed"))
				return false, nil
			}
			if err != nil {
				w.setWatcherError(err)
				w.log.Warn("filesystem watcher error", "error", err)
			}
		case event, ok := <-fsw.Events:
			if !ok {
				w.setUnavailable(cfg, errors.New("filesystem watcher event stream closed"))
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
				_ = w.tryWatchCreatedDirectory(ctx, set, event.Name, rootID)
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
			w.flush(ctx, batch)
		case <-rebalance.C:
			w.rebalance(ctx, set, cfg)
			w.updateStats(set, cfg)
		}
	}
}

func (w *Watcher) seedWatches(ctx context.Context, set *watchSet, cfg *config.Config) {
	topLevel := map[string][]string{}
	rootIDs := enabledRootIDs(cfg)
	perRootCandidateLimit := topLevelCandidateLimit(set, len(rootIDs))
	for _, root := range cfg.Roots {
		if !root.Enabled {
			continue
		}
		paths, err := expandRootPathsWithTimeout(ctx, root.Path)
		if err != nil {
			if isCancellation(ctx, err) {
				return
			}
			set.noteFailure(err)
			w.log.Warn("watch root unavailable", "root", root.ID, "path", root.Path, "error", err)
			continue
		}
		for _, path := range paths {
			if !set.add(ctx, path, root.ID, true) {
				if set.lastError != "" {
					w.log.Warn("watch root unavailable", "root", root.ID, "path", path, "error", set.lastError)
				}
				continue
			}
			entries, err := readDirWithTimeout(ctx, path, perRootCandidateLimit)
			if err != nil {
				if isCancellation(ctx, err) {
					return
				}
				set.noteFailure(err)
				w.log.Warn("watch root enumeration deferred", "root", root.ID, "path", path, "error", err)
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
	addCtx, cancel := context.WithTimeout(ctx, watchSeedAddTimeout)
	defer cancel()
	w.addRoundRobin(addCtx, set, topLevel, rootIDs, true)
	if addCtx.Err() != nil {
		w.log.Debug("watch seed registration deferred", "limit", set.pinnedLimit, "error", addCtx.Err())
	}
}

func (w *Watcher) addHotWatches(ctx context.Context, set *watchSet, cfg *config.Config) {
	if len(set.paths) >= set.limit {
		return
	}
	lookupCtx, cancel := context.WithTimeout(ctx, hotFolderLookupTimeout)
	defer cancel()
	hot, err := w.crawler.HotFolders(lookupCtx, enabledRootIDs(cfg), cfg.Watcher.ActivityHalfLife(), set.limit-len(set.paths))
	if err != nil {
		w.log.Debug("hot folder lookup deferred", "error", err)
		return
	}
	addCtx, addCancel := context.WithTimeout(ctx, hotWatchAddTimeout)
	defer addCancel()
	roots := make(map[string]config.RootConfig, len(cfg.Roots))
	for _, root := range cfg.Roots {
		if root.Enabled {
			roots[root.ID] = root
		}
	}
	candidates := map[string][]string{}
	for _, item := range hot {
		root, ok := roots[item.RootID]
		if !ok || !crawler.PathAllowedWithGlobalRules(root, cfg.Crawler, item.Path, "", true) {
			continue
		}
		candidates[item.RootID] = append(candidates[item.RootID], item.Path)
	}
	w.addRoundRobin(addCtx, set, candidates, enabledRootIDs(cfg), false)
}

func (w *Watcher) rebalance(ctx context.Context, set *watchSet, cfg *config.Config) {
	for path := range set.paths {
		if set.pinned[path] {
			continue
		}
		_ = set.fsw.Remove(path)
		delete(set.paths, path)
	}
	set.clearFailures()
	// Revisit configured roots as part of normal rebalancing. This lets a
	// temporarily disconnected share become watched again without a restart.
	w.seedWatches(ctx, set, cfg)
	w.addHotWatches(ctx, set, cfg)
	w.statsMu.Lock()
	w.stats.LastRebalanceUnix = time.Now().Unix()
	w.statsMu.Unlock()
	w.log.Debug("filesystem watcher rebalanced", "directories", len(set.paths), "pinned", len(set.pinned), "hot", len(set.paths)-len(set.pinned), "limit", set.limit)
}

func (w *Watcher) addRoundRobin(ctx context.Context, set *watchSet, candidates map[string][]string, rootIDs []string, pinned bool) {
	for {
		if ctx.Err() != nil {
			return
		}
		added := false
		for _, rootID := range rootIDs {
			if ctx.Err() != nil {
				return
			}
			paths := candidates[rootID]
			if len(paths) == 0 {
				continue
			}
			candidates[rootID] = paths[1:]
			if set.add(ctx, paths[0], rootID, pinned) {
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

func (s *watchSet) add(ctx context.Context, path, rootID string, pinned bool) bool {
	if ctx.Err() != nil {
		return false
	}
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
	if pinned && len(s.pinned) >= s.pinnedLimit {
		return false
	}
	info, err := statDirectoryWithTimeout(ctx, path)
	if err != nil || !info.IsDir() {
		if err != nil {
			s.noteFailure(err)
		} else {
			s.noteFailure(errors.New("path is not a directory"))
		}
		return false
	}
	if err := s.fsw.Add(path); err != nil {
		s.noteFailure(err)
		return false
	}
	s.paths[path] = rootID
	if pinned {
		s.pinned[path] = true
	}
	return true
}

func (s *watchSet) noteFailure(err error) {
	if err == nil {
		return
	}
	s.failures++
	s.lastError = err.Error()
}

func (s *watchSet) clearFailures() {
	s.failures = 0
	s.lastError = ""
}

func readDirWithTimeout(parent context.Context, path string, limit int) ([]os.DirEntry, error) {
	ctx, cancel := context.WithTimeout(parent, watchRootLookupTimeout)
	defer cancel()
	type result struct {
		entries []os.DirEntry
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		dir, err := os.Open(path)
		if err != nil {
			resultCh <- result{err: err}
			return
		}
		defer dir.Close()
		entries, err := dir.ReadDir(limit)
		if errors.Is(err, io.EOF) {
			err = nil
		}
		resultCh <- result{entries: entries, err: err}
	}()
	select {
	case result := <-resultCh:
		return result.entries, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func expandRootPathsWithTimeout(parent context.Context, path string) ([]string, error) {
	ctx, cancel := context.WithTimeout(parent, watchRootLookupTimeout)
	defer cancel()
	type result struct {
		paths []string
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		paths, err := crawler.ExpandRootPaths(path)
		resultCh <- result{paths: paths, err: err}
	}()
	select {
	case result := <-resultCh:
		return result.paths, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func statDirectoryWithTimeout(parent context.Context, path string) (os.FileInfo, error) {
	ctx, cancel := context.WithTimeout(parent, watchRootLookupTimeout)
	defer cancel()
	type result struct {
		info os.FileInfo
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		info, err := os.Stat(path)
		resultCh <- result{info: info, err: err}
	}()
	select {
	case result := <-resultCh:
		return result.info, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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

func pinnedWatchLimit(limit, rootCount int) int {
	if limit <= 0 || rootCount <= 0 {
		return 0
	}
	reserved := (limit * 3) / 5
	if reserved < rootCount {
		reserved = rootCount
	}
	if reserved > limit {
		return limit
	}
	return reserved
}

func topLevelCandidateLimit(set *watchSet, rootCount int) int {
	if rootCount <= 0 || set.pinnedLimit <= len(set.pinned) {
		return 1
	}
	return max(1, (set.pinnedLimit-len(set.pinned))/rootCount)
}

func (w *Watcher) updateStats(set *watchSet, cfg *config.Config) {
	pendingRoots, pendingPaths := w.hints.Stats()
	w.statsMu.Lock()
	lastEvent, lastRebalance := w.stats.LastEventUnix, w.stats.LastRebalanceUnix
	errText := ""
	if set.failures > 0 {
		errText = fmt.Sprintf("%d watch path(s) unavailable; last error: %s", set.failures, set.lastError)
	}
	w.stats = Stats{Enabled: cfg.Watcher.Enabled, Running: true, Error: errText, WatchedDirectories: len(set.paths), MaxDirectories: set.limit, PinnedDirectories: len(set.pinned), HotDirectories: len(set.paths) - len(set.pinned), PendingHintRoots: pendingRoots, PendingHintPaths: pendingPaths, LastEventUnix: lastEvent, LastRebalanceUnix: lastRebalance}
	w.statsMu.Unlock()
}

func (w *Watcher) setUnavailable(cfg *config.Config, err error) {
	w.statsMu.Lock()
	w.stats = Stats{Enabled: cfg.Watcher.Enabled, Running: false, MaxDirectories: cfg.Watcher.MaxWatchedDirectories, Error: err.Error()}
	w.statsMu.Unlock()
}

func (w *Watcher) setWatcherError(err error) {
	w.statsMu.Lock()
	w.stats.Error = err.Error()
	w.statsMu.Unlock()
}

func (w *Watcher) recordEvent() {
	w.statsMu.Lock()
	w.stats.LastEventUnix = time.Now().Unix()
	w.statsMu.Unlock()
}

func isCancellation(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled)
}

func (w *Watcher) shouldIgnore(path string) bool {
	if w.ignore == "" || w.ignore == "." {
		return false
	}
	rel, err := filepath.Rel(w.ignore, filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (w *Watcher) tryWatchCreatedDirectory(ctx context.Context, set *watchSet, path string, rootID string) error {
	if w.shouldIgnore(path) {
		return nil
	}
	set.add(ctx, path, rootID, false)
	return nil
}

func (w *Watcher) flush(ctx context.Context, batch map[string]map[string]dirtyPath) {
	w.hints.submit(ctx, batch)
}

func (w *Watcher) processHints(ctx context.Context, rootID string, paths map[string]dirtyPath) bool {
	cfg := w.snapshot()
	limit := cfg.Watcher.MaxDirtyPathsPerFlush
	activity := make([]string, 0, len(paths))
	for _, dirty := range paths {
		activity = append(activity, dirty.path)
	}
	w.crawler.RecordFolderActivity(ctx, rootID, activity, 5)
	if limit > 0 && len(paths) > limit {
		target := commonDirtyParent(paths)
		if target != "" {
			w.log.Warn("dirty path limit reached; crawling affected scope", "root", rootID, "paths", len(paths), "limit", limit, "path", target)
			run, err := w.crawler.CrawlHint(ctx, rootID, target, false)
			if err != nil {
				if isCancellation(ctx, err) {
					return false
				}
				w.log.Warn("affected scope crawl failed", "root", rootID, "path", target, "error", err)
				return true
			}
			return run.Status != "already_running" && run.Status != "maintenance_paused"
		}
		w.log.Warn("dirty path limit reached; falling back to root crawl", "root", rootID, "limit", limit)
		run, err := w.crawler.CrawlRoot(ctx, rootID)
		if err != nil {
			if isCancellation(ctx, err) {
				return false
			}
			w.log.Warn("affected root crawl failed", "root", rootID, "error", err)
			return true
		}
		return run.Status != "already_running" && run.Status != "maintenance_paused"
	}
	for _, dirty := range paths {
		run, err := w.crawler.CrawlHint(ctx, rootID, dirty.path, dirty.removed)
		if err != nil {
			if isCancellation(ctx, err) {
				return false
			}
			w.log.Warn("hint crawl failed", "root", rootID, "path", dirty.path, "error", err)
			continue
		}
		if run.Status == "already_running" || run.Status == "maintenance_paused" {
			return false
		}
	}
	return true
}

// hintQueue decouples native filesystem-event intake from crawl work. A busy
// root gets one ordered worker; new events coalesce behind it instead of
// blocking the watcher loop or being discarded while a full crawl owns root.
type hintQueue struct {
	watcher *Watcher
	mu      sync.Mutex
	pending map[string]map[string]dirtyPath
	running map[string]bool
}

func newHintQueue(w *Watcher) *hintQueue {
	return &hintQueue{watcher: w, pending: map[string]map[string]dirtyPath{}, running: map[string]bool{}}
}

func (q *hintQueue) submit(ctx context.Context, batch map[string]map[string]dirtyPath) {
	var start []string
	q.mu.Lock()
	for rootID, paths := range batch {
		if len(paths) == 0 {
			continue
		}
		mergeDirtyPaths(q.pending, rootID, paths)
		if !q.running[rootID] {
			q.running[rootID] = true
			start = append(start, rootID)
		}
	}
	q.mu.Unlock()
	q.publishStats()
	for _, rootID := range start {
		go q.run(ctx, rootID)
	}
}

func (q *hintQueue) run(ctx context.Context, rootID string) {
	for {
		paths := q.take(rootID)
		if len(paths) == 0 {
			return
		}
		if q.watcher.processHints(ctx, rootID, paths) || ctx.Err() != nil {
			continue
		}
		// The root is currently busy or under maintenance. Retain the hint and
		// wait briefly rather than repeatedly competing for the root lock.
		q.requeue(rootID, paths)
		waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		q.watcher.crawler.WaitRoot(waitCtx, rootID)
		cancel()
		if ctx.Err() != nil {
			return
		}
		// Maintenance has no active root to wait on. Yield before retrying so a
		// deliberate pause cannot turn into a hot loop.
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (q *hintQueue) take(rootID string) map[string]dirtyPath {
	q.mu.Lock()
	paths := q.pending[rootID]
	delete(q.pending, rootID)
	if len(paths) == 0 {
		delete(q.running, rootID)
	}
	q.mu.Unlock()
	q.publishStats()
	return paths
}

func (q *hintQueue) requeue(rootID string, paths map[string]dirtyPath) {
	q.mu.Lock()
	mergeDirtyPaths(q.pending, rootID, paths)
	q.mu.Unlock()
	q.publishStats()
}

func mergeDirtyPaths(dst map[string]map[string]dirtyPath, rootID string, src map[string]dirtyPath) {
	if dst[rootID] == nil {
		dst[rootID] = make(map[string]dirtyPath, len(src))
	}
	for path, dirty := range src {
		dst[rootID][path] = dirty
	}
}

func (q *hintQueue) Stats() (int, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.statsLocked()
}

func (q *hintQueue) statsLocked() (int, int) {
	paths := 0
	for _, dirty := range q.pending {
		paths += len(dirty)
	}
	return len(q.pending), paths
}

func (q *hintQueue) publishStats() {
	roots, paths := q.Stats()
	q.watcher.statsMu.Lock()
	q.watcher.stats.PendingHintRoots = roots
	q.watcher.stats.PendingHintPaths = paths
	q.watcher.statsMu.Unlock()
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

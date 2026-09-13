package crawler

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	pathmatch "path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/process"

	"qindexer/internal/catalog"
	"qindexer/internal/config"
	"qindexer/internal/extract"
	"qindexer/internal/filemeta"
)

var (
	errRootTraversalIncomplete = errors.New("root traversal incomplete")
	errRootTraversalPartial    = errors.New("root traversal partially completed")
)

type Crawler struct {
	cfg                 *config.Config
	cfgMu               sync.RWMutex
	cat                 *catalog.Catalog
	log                 *slog.Logger
	mu                  sync.Mutex
	run                 map[string]context.CancelFunc
	done                map[string]chan struct{}
	startedAt           time.Time
	activeCrawls        int64
	filesStatted        int64
	bytesStatted        int64
	dirsRead            int64
	hintCrawls          int64
	fullCrawls          int64
	initialCrawls       int64
	lastActivity        int64
	pausedUntil         int64
	nextFullCrawl       int64
	maintenance         int64
	maintenanceMu       sync.Mutex
	maintenancePrevious int64
	maintenanceTarget   int64
	pressureMu          sync.RWMutex
	pressure            adaptivePressure
	contentJobs         chan backgroundJob
	ocrJobs             chan backgroundJob
	hashJobs            chan backgroundJob
	retry               map[string]bool
	logState            map[string]suppressedLog
	backgroundMu        sync.Mutex
	backgroundCtx       context.Context
	backgroundEnd       context.CancelFunc
	backgroundN         int64
	backgroundZero      chan struct{}
}

type backgroundJob struct {
	id, rootID, path, signature string
	size                        int64
}

type suppressedLog struct {
	message    string
	count      int
	lastLogged time.Time
}

type adaptivePressure struct {
	Paused          bool
	Reason          string
	CPUPercent      float64
	DiskBusyPercent float64
	HealthySamples  int
}

func New(cfg *config.Config, cat *catalog.Catalog, log *slog.Logger) *Crawler {
	snapshot := config.Clone(cfg)
	zero := make(chan struct{})
	close(zero)
	return &Crawler{cfg: snapshot, cat: cat, log: log, run: map[string]context.CancelFunc{}, done: map[string]chan struct{}{}, retry: map[string]bool{}, logState: map[string]suppressedLog{}, startedAt: time.Now().UTC(), contentJobs: make(chan backgroundJob, snapshot.Crawler.ContentExtraction.QueueSize), ocrJobs: make(chan backgroundJob, snapshot.Crawler.OCR.QueueSize), hashJobs: make(chan backgroundJob, snapshot.Crawler.Hashing.QueueSize), backgroundZero: zero}
}

func (c *Crawler) snapshot() *config.Config {
	c.cfgMu.RLock()
	defer c.cfgMu.RUnlock()
	return c.cfg
}

// ApplyConfig publishes an immutable configuration snapshot to future work.
// Queue and worker-count changes take effect after restart because those
// channels and goroutines are intentionally fixed for the process lifetime.
func (c *Crawler) ApplyConfig(cfg *config.Config) {
	c.cfgMu.Lock()
	c.cfg = config.Clone(cfg)
	c.cfgMu.Unlock()
}

type Stats struct {
	StartedAt         time.Time `json:"started_at"`
	UptimeSeconds     float64   `json:"uptime_seconds"`
	ActiveCrawls      int64     `json:"active_crawls"`
	ActiveRoots       []string  `json:"active_roots"`
	InitialCrawls     int64     `json:"initial_crawls"`
	FilesStatted      int64     `json:"files_statted"`
	DirectoriesRead   int64     `json:"directories_read"`
	BytesStatted      int64     `json:"bytes_statted"`
	FilesPerSecond    float64   `json:"files_per_second"`
	DirsPerSecond     float64   `json:"directories_per_second"`
	BytesPerSecond    float64   `json:"bytes_per_second"`
	HintCrawls        int64     `json:"hint_crawls"`
	FullCrawls        int64     `json:"full_crawls"`
	LastActivityUnix  int64     `json:"last_activity_unix"`
	Paused            bool      `json:"paused"`
	PausedUntilUnix   int64     `json:"paused_until_unix"`
	PauseReason       string    `json:"pause_reason,omitempty"`
	AdaptivePaused    bool      `json:"adaptive_paused"`
	CPUPercent        float64   `json:"cpu_percent"`
	DiskBusyPercent   float64   `json:"disk_busy_percent"`
	NextFullCrawlUnix int64     `json:"next_full_crawl_unix"`
	ContentQueueDepth int       `json:"content_queue_depth"`
	OCRQueueDepth     int       `json:"ocr_queue_depth"`
	HashQueueDepth    int       `json:"hash_queue_depth"`
}

func (c *Crawler) Stats() Stats {
	uptime := time.Since(c.startedAt).Seconds()
	if uptime <= 0 {
		uptime = 1
	}
	files := atomic.LoadInt64(&c.filesStatted)
	dirs := atomic.LoadInt64(&c.dirsRead)
	bytes := atomic.LoadInt64(&c.bytesStatted)
	pausedUntil := atomic.LoadInt64(&c.pausedUntil)
	initialCrawls := atomic.LoadInt64(&c.initialCrawls)
	pausedReason := ""
	pressure := c.adaptiveStatus()
	if scheduledUntil, active := c.scheduledPauseUntil(time.Now()); active && scheduledUntil.Unix() > pausedUntil {
		pausedUntil = scheduledUntil.Unix()
		pausedReason = "schedule"
	} else if pausedUntil > time.Now().Unix() {
		pausedReason = "manual"
	}
	if pressure.Paused && initialCrawls == 0 {
		pausedReason = "adaptive_" + pressure.Reason
	}
	return Stats{
		StartedAt: c.startedAt, UptimeSeconds: uptime,
		ActiveCrawls: atomic.LoadInt64(&c.activeCrawls), InitialCrawls: initialCrawls,
		ActiveRoots:  c.ActiveRoots(),
		FilesStatted: files, DirectoriesRead: dirs, BytesStatted: bytes,
		FilesPerSecond: float64(files) / uptime, DirsPerSecond: float64(dirs) / uptime,
		BytesPerSecond: float64(bytes) / uptime,
		HintCrawls:     atomic.LoadInt64(&c.hintCrawls), FullCrawls: atomic.LoadInt64(&c.fullCrawls),
		LastActivityUnix:  atomic.LoadInt64(&c.lastActivity),
		Paused:            pausedUntil > time.Now().Unix() || (pressure.Paused && initialCrawls == 0),
		PausedUntilUnix:   pausedUntil,
		PauseReason:       pausedReason,
		AdaptivePaused:    pressure.Paused,
		CPUPercent:        pressure.CPUPercent,
		DiskBusyPercent:   pressure.DiskBusyPercent,
		NextFullCrawlUnix: atomic.LoadInt64(&c.nextFullCrawl),
		ContentQueueDepth: len(c.contentJobs),
		OCRQueueDepth:     len(c.ocrJobs),
		HashQueueDepth:    len(c.hashJobs),
	}
}

func (c *Crawler) Pause(duration time.Duration) time.Time {
	if duration <= 0 {
		duration = 5 * time.Minute
	}
	until := time.Now().Add(duration).UTC()
	atomic.StoreInt64(&c.pausedUntil, until.Unix())
	return until
}

func (c *Crawler) MaintenancePause(duration time.Duration) (time.Time, func()) {
	if duration <= 0 {
		duration = 10 * time.Minute
	}
	previous := atomic.LoadInt64(&c.pausedUntil)
	target := time.Now().Add(duration).UTC().Unix()
	for {
		current := atomic.LoadInt64(&c.pausedUntil)
		if current >= target {
			return time.Unix(current, 0).UTC(), func() {}
		}
		if atomic.CompareAndSwapInt64(&c.pausedUntil, current, target) {
			previous = current
			break
		}
	}
	return time.Unix(target, 0).UTC(), func() {
		atomic.CompareAndSwapInt64(&c.pausedUntil, target, previous)
	}
}

func (c *Crawler) BeginMaintenance(duration time.Duration) (time.Time, func()) {
	if duration <= 0 {
		duration = 10 * time.Minute
	}
	target := time.Now().Add(duration).UTC().Unix()
	c.maintenanceMu.Lock()
	if atomic.LoadInt64(&c.maintenance) == 0 {
		c.maintenancePrevious = atomic.LoadInt64(&c.pausedUntil)
		c.maintenanceTarget = target
	} else if target > c.maintenanceTarget {
		c.maintenanceTarget = target
	}
	if c.maintenanceTarget > atomic.LoadInt64(&c.pausedUntil) {
		atomic.StoreInt64(&c.pausedUntil, c.maintenanceTarget)
	}
	atomic.AddInt64(&c.maintenance, 1)
	until := c.maintenanceTarget
	c.maintenanceMu.Unlock()
	return time.Unix(until, 0).UTC(), func() {
		c.maintenanceMu.Lock()
		remaining := atomic.AddInt64(&c.maintenance, -1)
		if remaining == 0 {
			if atomic.LoadInt64(&c.pausedUntil) == c.maintenanceTarget {
				atomic.StoreInt64(&c.pausedUntil, c.maintenancePrevious)
			}
			c.maintenanceTarget = 0
			c.maintenancePrevious = 0
		}
		c.maintenanceMu.Unlock()
	}
}

func (c *Crawler) Resume() {
	atomic.StoreInt64(&c.pausedUntil, 0)
}

func (c *Crawler) CrawlRoot(ctx context.Context, rootID string) (catalog.CrawlRun, error) {
	root, ok := c.rootByID(rootID)
	if !ok {
		return catalog.CrawlRun{}, os.ErrNotExist
	}
	return c.crawl(ctx, root, true)
}

func (c *Crawler) IsRootRunning(rootID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.run[rootID] != nil
}

func (c *Crawler) InMaintenance() bool {
	return atomic.LoadInt64(&c.maintenance) > 0
}

func (c *Crawler) CancelRoot(rootID string) bool {
	c.mu.Lock()
	cancel := c.run[rootID]
	c.mu.Unlock()
	if cancel == nil {
		return false
	}
	c.log.Info("crawler root stop requested", "root", rootID)
	cancel()
	c.cancelBackgroundWork()
	return true
}

// CancelEnrichment interrupts in-flight content extraction, OCR, and hashing.
// It is intentionally global because the worker pool is shared between roots.
func (c *Crawler) CancelEnrichment() { c.cancelBackgroundWork() }

func (c *Crawler) CancelAll() []string {
	c.mu.Lock()
	type activeCancel struct {
		rootID string
		cancel context.CancelFunc
	}
	cancels := make([]activeCancel, 0, len(c.run))
	for rootID, cancel := range c.run {
		cancels = append(cancels, activeCancel{rootID: rootID, cancel: cancel})
	}
	c.mu.Unlock()
	roots := make([]string, 0, len(cancels))
	for _, active := range cancels {
		roots = append(roots, active.rootID)
	}
	if len(cancels) > 0 {
		c.log.Info("crawler stop requested", "active_roots", roots)
	}
	for _, active := range cancels {
		active.cancel()
	}
	c.cancelBackgroundWork()
	return roots
}

func (c *Crawler) CancelAllAndPause(duration time.Duration) (time.Time, []string) {
	until := c.Pause(duration)
	activeRoots := c.CancelAll()
	return until, activeRoots
}

func (c *Crawler) WaitAll(ctx context.Context) bool {
	c.mu.Lock()
	done := make([]chan struct{}, 0, len(c.done))
	for _, ch := range c.done {
		done = append(done, ch)
	}
	c.mu.Unlock()
	for _, ch := range done {
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
	return c.WaitEnrichment(ctx)
}

func (c *Crawler) WaitEnrichment(ctx context.Context) bool {
	c.backgroundMu.Lock()
	backgroundZero := c.backgroundZero
	c.backgroundMu.Unlock()
	select {
	case <-backgroundZero:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Crawler) beginBackgroundWork(parent context.Context) (context.Context, func()) {
	c.backgroundMu.Lock()
	if c.backgroundCtx == nil {
		c.backgroundCtx, c.backgroundEnd = context.WithCancel(parent)
	}
	if c.backgroundN == 0 {
		c.backgroundZero = make(chan struct{})
	}
	c.backgroundN++
	ctx := c.backgroundCtx
	c.backgroundMu.Unlock()
	return ctx, func() {
		c.backgroundMu.Lock()
		c.backgroundN--
		if c.backgroundN == 0 {
			close(c.backgroundZero)
		}
		c.backgroundMu.Unlock()
	}
}

func (c *Crawler) cancelBackgroundWork() {
	c.backgroundMu.Lock()
	if c.backgroundEnd != nil {
		c.backgroundEnd()
	}
	c.backgroundCtx = nil
	c.backgroundEnd = nil
	c.backgroundMu.Unlock()
	c.discardQueuedEnrichment()
	resetCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if c.cat != nil {
		if err := c.cat.ResetQueuedBackground(resetCtx); err != nil {
			c.log.Warn("could not release cancelled enrichment jobs", "error", err)
		}
	}
}

// A cancelled crawl can have queued work using paths from before a root-path
// repair. Drop those jobs; the refill loop reads the repaired catalog paths.
func (c *Crawler) discardQueuedEnrichment() {
	drain := func(jobs chan backgroundJob) {
		for {
			select {
			case <-jobs:
			default:
				return
			}
		}
	}
	drain(c.contentJobs)
	drain(c.ocrJobs)
	drain(c.hashJobs)
}

func (c *Crawler) WaitRoot(ctx context.Context, rootID string) bool {
	c.mu.Lock()
	done := c.done[rootID]
	c.mu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Crawler) ActiveRoots() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	roots := make([]string, 0, len(c.run))
	for rootID := range c.run {
		roots = append(roots, rootID)
	}
	return roots
}

// beginRootWork serializes every crawl operation for a root, including
// filesystem-watcher hints, so stop and maintenance operations see all work.
func (c *Crawler) beginRootWork(parent context.Context, rootID string) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	c.mu.Lock()
	if c.run[rootID] != nil {
		c.mu.Unlock()
		cancel()
		return nil, nil, false
	}
	c.run[rootID] = cancel
	c.done[rootID] = done
	c.mu.Unlock()
	return ctx, func() {
		cancel()
		c.mu.Lock()
		delete(c.run, rootID)
		delete(c.done, rootID)
		close(done)
		c.mu.Unlock()
	}, true
}

func (c *Crawler) CrawlHint(ctx context.Context, rootID string, path string, removed bool) (catalog.CrawlRun, error) {
	root, ok := c.rootByID(rootID)
	if !ok {
		return catalog.CrawlRun{}, os.ErrNotExist
	}
	return c.crawlHint(ctx, root, path, removed)
}

func (c *Crawler) CrawlAll(ctx context.Context) []catalog.CrawlRun {
	return c.crawlRoots(ctx, false, func(config.RootConfig) bool { return true })
}

func (c *Crawler) CrawlUnindexedRoots(ctx context.Context) []catalog.CrawlRun {
	states, err := c.cat.RootStates(ctx)
	if err != nil {
		c.log.Warn("could not read root crawl state; running initial reconciliation", "error", err)
		return c.CrawlAll(ctx)
	}
	return c.crawlRoots(ctx, true, func(root config.RootConfig) bool {
		state, exists := states[root.ID]
		return needsInitialCrawl(root, exists, state)
	})
}

func needsInitialCrawl(root config.RootConfig, exists bool, state catalog.RootState) bool {
	return root.Enabled && (!exists || state.LastSuccessfulCrawlAt == nil)
}

func (c *Crawler) crawlRoots(ctx context.Context, initial bool, include func(config.RootConfig) bool) []catalog.CrawlRun {
	cfg := c.snapshot()
	sem := make(chan struct{}, cfg.Crawler.RootParallelism)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var runs []catalog.CrawlRun
	selected := 0
	for _, root := range cfg.Roots {
		if !root.Enabled || !include(root) {
			continue
		}
		selected++
		root := root
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			run, _ := c.crawl(ctx, root, initial)
			mu.Lock()
			runs = append(runs, run)
			mu.Unlock()
		}()
	}
	if selected == 0 {
		c.log.Info("crawl pass skipped", "initial", initial, "reason", "no_enabled_roots_selected")
	}
	wg.Wait()
	c.scheduleRetries(ctx, runs)
	return runs
}

func (c *Crawler) scheduleRetries(ctx context.Context, runs []catalog.CrawlRun) {
	for _, run := range runs {
		if run.Status != "unreachable" && run.Status != "partial" {
			if run.RootID != "" && (run.Status == "ok" || run.Status == "hint_ok") {
				c.clearSuppressedLogsForRoot(run.RootID)
			}
			continue
		}
		root, ok := c.rootByID(run.RootID)
		if !ok || !root.Enabled {
			continue
		}
		c.scheduleRetry(ctx, root, 30*time.Second, run.ErrorMessage)
	}
}

func (c *Crawler) scheduleRetry(ctx context.Context, root config.RootConfig, delay time.Duration, reason string) {
	c.mu.Lock()
	if c.retry[root.ID] {
		c.mu.Unlock()
		return
	}
	c.retry[root.ID] = true
	c.mu.Unlock()
	c.logSuppressed("retry:"+root.ID, "root crawl retry scheduled", reason, 5*time.Minute, "root", root.ID, "delay", delay, "reason", reason)
	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			c.mu.Lock()
			delete(c.retry, root.ID)
			c.mu.Unlock()
			return
		case <-timer.C:
		}
		c.mu.Lock()
		delete(c.retry, root.ID)
		c.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		currentRoot, exists := c.rootByID(root.ID)
		if !exists || !currentRoot.Enabled {
			return
		}
		run, err := c.crawl(ctx, currentRoot, false)
		if err != nil {
			c.logSuppressed("retry-failed:"+root.ID, "root crawl retry failed", fmt.Sprint(err), 5*time.Minute, "root", root.ID, "status", run.Status, "error", err)
		}
		if run.Status == "unreachable" || run.Status == "partial" {
			c.scheduleRetry(ctx, root, delay, run.ErrorMessage)
		} else if run.Status == "ok" {
			c.clearSuppressedLogsForRoot(root.ID)
		}
	}()
}

func (c *Crawler) logSuppressed(key, event, message string, interval time.Duration, attrs ...any) {
	now := time.Now()
	c.mu.Lock()
	state := c.logState[key]
	suppressed := 0
	shouldLog := state.message != message || state.lastLogged.IsZero() || now.Sub(state.lastLogged) >= interval
	if shouldLog {
		suppressed = state.count
		state = suppressedLog{message: message, count: 0, lastLogged: now}
	} else {
		state.count++
	}
	c.logState[key] = state
	c.mu.Unlock()
	if !shouldLog {
		return
	}
	if suppressed > 0 {
		attrs = append(attrs, "suppressed_repeats", suppressed)
	}
	c.log.Warn(event, attrs...)
}

func (c *Crawler) clearSuppressedLogsForRoot(rootID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefixes := []string{"retry:" + rootID, "retry-failed:" + rootID, "crawl-unreachable:" + rootID, "root-stat:" + rootID + ":", "dir-read:" + rootID + ":"}
	for key := range c.logState {
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				delete(c.logState, key)
				break
			}
		}
	}
}

func (c *Crawler) Loop(ctx context.Context) {
	var workers sync.WaitGroup
	defer func() {
		c.CancelAll()
		// A remote filesystem read or parser can be blocked below Go's
		// cancellation layer. The service owns process shutdown and must not be
		// held hostage by that work; the OS closes any stragglers on exit.
		go func() { workers.Wait() }()
	}()
	startCrawlerRoutine(ctx, &workers, c.log, c.adaptiveMonitor)
	c.startBackgroundWorkers(ctx, &workers)
	startCrawlerRoutine(ctx, &workers, c.log, c.refillBackgroundLoop)
	scanInterval := c.snapshot().Crawler.ScanInterval()
	c.log.Info("crawler loop started", "scan_interval", scanInterval)
	ticker := time.NewTicker(scanInterval)
	defer ticker.Stop()
	atomic.StoreInt64(&c.nextFullCrawl, time.Now().Add(scanInterval).Unix())
	if c.waitIfPaused(ctx, true) != nil {
		c.log.Info("crawler loop stopped before initial crawl", "reason", "context_cancelled")
		return
	}
	// A restart should preserve existing index state. Only new or incomplete
	// roots need an initial crawl; watcher events cover ordinary changes.
	c.CrawlUnindexedRoots(ctx)
	for {
		select {
		case <-ctx.Done():
			c.log.Info("crawler loop stopped", "reason", "context_cancelled")
			return
		case <-ticker.C:
			atomic.StoreInt64(&c.nextFullCrawl, time.Now().Add(c.snapshot().Crawler.ScanInterval()).Unix())
			if c.waitIfPaused(ctx, true) != nil {
				c.log.Info("crawler loop stopped while waiting to run scheduled crawl", "reason", "context_cancelled")
				return
			}
			c.CrawlAll(ctx)
		}
	}
}

func startCrawlerRoutine(ctx context.Context, workers *sync.WaitGroup, log *slog.Logger, fn func(context.Context)) {
	workers.Add(1)
	go func() {
		defer workers.Done()
		defer func() {
			if value := recover(); value != nil {
				log.Error("crawler routine panic recovered", "panic", value, "stack", string(debug.Stack()))
			}
		}()
		fn(ctx)
	}()
}

func (c *Crawler) refillBackgroundLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		c.refillBackground(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Crawler) refillBackground(ctx context.Context) {
	// Structural indexing has priority. Do not compete with NAS enumeration,
	// metadata writes, or recovery work until every active crawl is finished.
	if atomic.LoadInt64(&c.activeCrawls) > 0 || c.InMaintenance() {
		return
	}
	content := c.snapshot().Crawler.ContentExtraction
	if rootIDs := c.rootIDsFor(func(root config.RootConfig) bool { return root.ContentExtractionEnabled(content.Enabled) }); len(rootIDs) > 0 {
		c.fillBackground(ctx, rootIDs, c.contentJobs, content.MaxFileSizeMB<<20, c.cat.ClaimPendingContent, "content")
	}
	ocr := c.snapshot().Crawler.OCR
	if rootIDs := c.rootIDsFor(func(root config.RootConfig) bool {
		return root.ContentExtractionEnabled(content.Enabled) && root.OCREnabled(ocr.Enabled)
	}); len(rootIDs) > 0 {
		c.fillBackground(ctx, rootIDs, c.ocrJobs, ocr.MaxFileSizeMB<<20, c.cat.ClaimPendingOCR, "OCR")
	}
	hashing := c.snapshot().Crawler.Hashing
	if rootIDs := c.rootIDsFor(func(root config.RootConfig) bool { return root.HashingEnabled(hashing.Enabled) }); len(rootIDs) > 0 {
		c.fillBackground(ctx, rootIDs, c.hashJobs, hashing.MaxFileSizeMB<<20, c.cat.ClaimPendingHashes, "hash")
	}
}

func (c *Crawler) waitForEnrichmentWindow(ctx context.Context) error {
	for atomic.LoadInt64(&c.activeCrawls) > 0 || c.InMaintenance() {
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return c.waitIfPaused(ctx, true)
}

type backgroundClaim func(context.Context, []string, int, int64) ([]catalog.BackgroundCandidate, error)

// fillBackground shares capacity across roots so a sprawling root cannot starve
// a smaller root's extraction, OCR, or hashing backlog.
func (c *Crawler) fillBackground(ctx context.Context, rootIDs []string, jobs chan<- backgroundJob, maxSize int64, claim backgroundClaim, kind string) {
	if ctx.Err() != nil {
		return
	}
	available := cap(jobs) - len(jobs)
	if available <= 0 {
		return
	}
	perRoot := max(1, available/len(rootIDs))
	batches := make([][]catalog.BackgroundCandidate, 0, len(rootIDs))
	for _, rootID := range rootIDs {
		if available <= 0 {
			return
		}
		limit := min(perRoot, available)
		items, err := claim(ctx, []string{rootID}, limit, maxSize)
		if err != nil {
			c.log.Debug(kind+" queue refill failed", "root", rootID, "error", err)
			continue
		}
		batches = append(batches, items)
		available -= len(items)
	}
	// Interleave roots so one worker makes progress on every root promptly.
	for pending := true; pending; {
		pending = false
		for i := range batches {
			if len(batches[i]) == 0 {
				continue
			}
			item := batches[i][0]
			batches[i] = batches[i][1:]
			select {
			case jobs <- backgroundJob{id: item.ID, rootID: item.RootID, path: item.Path, signature: item.Signature, size: item.Size}:
			case <-ctx.Done():
				return
			}
			pending = true
		}
	}
}

func (c *Crawler) crawl(ctx context.Context, root config.RootConfig, initial bool) (catalog.CrawlRun, error) {
	if atomic.LoadInt64(&c.maintenance) > 0 {
		return catalog.CrawlRun{RootID: root.ID, Status: "maintenance_paused", ErrorMessage: "crawler maintenance in progress"}, nil
	}
	var started bool
	ctx, finish, started := c.beginRootWork(ctx, root.ID)
	if !started {
		return catalog.CrawlRun{RootID: root.ID, Status: "already_running"}, nil
	}
	defer finish()
	atomic.AddInt64(&c.activeCrawls, 1)
	atomic.AddInt64(&c.fullCrawls, 1)
	if initial {
		atomic.AddInt64(&c.initialCrawls, 1)
	}
	atomic.StoreInt64(&c.lastActivity, time.Now().Unix())
	defer func() {
		if initial {
			atomic.AddInt64(&c.initialCrawls, -1)
		}
		atomic.AddInt64(&c.activeCrawls, -1)
	}()

	generation, err := c.cat.NextGeneration(ctx, root.ID)
	if err != nil {
		return catalog.CrawlRun{}, err
	}
	run := catalog.CrawlRun{
		ID:         uuid.NewString(),
		RootID:     root.ID,
		Generation: generation,
		StartedAt:  time.Now().UTC(),
		Status:     "running",
	}
	if err := c.cat.StartCrawl(ctx, run); err != nil {
		return run, err
	}
	paths, err := ExpandRootPaths(root.Path)
	if err != nil {
		run.Status = "unreachable"
		run.ErrorMessage = err.Error()
		run.Errors = 1
		_ = c.finishCrawl(ctx, run)
		return run, err
	}

	settings := c.snapshot().Crawler
	jobs := make(chan string, settings.MetadataQueueSize)
	priorityLimit := max(10_000, settings.MetadataQueueSize*max(1, settings.MetadataWorkerCount)*4)
	priorityPaths, priorityErr := c.cat.MissingDocumentPaths(ctx, root.ID, priorityLimit)
	if priorityErr != nil {
		c.log.Warn("missing-path priority lookup failed", "root", root.ID, "error", priorityErr)
	}
	priorityMissing := make(map[string]struct{}, len(priorityPaths))
	for _, path := range priorityPaths {
		priorityMissing[path] = struct{}{}
	}
	var filesSeen, filesAdded, filesUpdated, filesUnchanged, errorsCount int64
	var workers sync.WaitGroup
	batchSize := max(1, settings.IndexBatchSize/max(1, settings.MetadataWorkerCount))
	for i := 0; i < settings.MetadataWorkerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			batch := make([]catalog.Document, 0, batchSize)
			flush := func() bool {
				if len(batch) == 0 {
					return true
				}
				results, err := c.cat.UpsertDocuments(ctx, batch)
				if err != nil {
					if !isCrawlerCancellation(ctx, err) {
						atomic.AddInt64(&errorsCount, int64(len(batch)))
						c.log.Warn("metadata batch index failed", "root", root.ID, "entries", len(batch), "error", err)
						batch = batch[:0]
						return true
					}
					return false
				}
				for _, res := range results {
					atomic.AddInt64(&filesSeen, 1)
					if res.Added {
						atomic.AddInt64(&filesAdded, 1)
					} else if res.Updated {
						atomic.AddInt64(&filesUpdated, 1)
					} else if res.Unchanged {
						atomic.AddInt64(&filesUnchanged, 1)
					}
				}
				batch = batch[:0]
				return true
			}
			for path := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if c.waitIfPaused(ctx, !initial) != nil {
					return
				}
				doc, indexable, err := c.describePath(root, path, generation)
				if err != nil {
					if isCrawlerCancellation(ctx, err) {
						return
					}
					if _, priority := priorityMissing[path]; priority && errors.Is(err, fs.ErrNotExist) {
						// It was missing last pass and is still absent. Leave the
						// existing missing record alone without turning recovery into
						// an error storm.
						continue
					}
					atomic.AddInt64(&errorsCount, 1)
					c.log.Warn("metadata index failed", "root", root.ID, "path", path, "error", err)
					continue
				}
				if !indexable {
					atomic.AddInt64(&filesSeen, 1)
					atomic.AddInt64(&filesUnchanged, 1)
					continue
				}
				batch = append(batch, doc)
				if len(batch) >= batchSize {
					if !flush() {
						return
					}
				}
			}
			flush()
		}()
	}
	var walkErr error
	for _, path := range priorityPaths {
		if err := sendPath(ctx, jobs, path); err != nil {
			walkErr = err
			break
		}
	}
	if walkErr == nil {
		walkErr = c.walkPaths(ctx, root, paths, jobs, &errorsCount, generation, true, !initial)
	}
	close(jobs)
	workers.Wait()

	run.FilesSeen = atomic.LoadInt64(&filesSeen)
	run.FilesAdded = atomic.LoadInt64(&filesAdded)
	run.FilesUpdated = atomic.LoadInt64(&filesUpdated)
	run.FilesUnchanged = atomic.LoadInt64(&filesUnchanged)
	run.Errors = atomic.LoadInt64(&errorsCount)
	if walkErr != nil {
		if errors.Is(walkErr, context.Canceled) {
			run.Status = "cancelled"
			// Metadata batches intentionally do not commit after cancellation.
			// Do not resume through directory checkpoints that may have been
			// written after files were queued but before their batch committed.
			if err := c.cat.ClearRootCheckpoints(context.Background(), root.ID); err != nil {
				run.ErrorMessage = err.Error()
			}
		} else if errors.Is(walkErr, errRootTraversalIncomplete) {
			run.Status = "unreachable"
			run.ErrorMessage = walkErr.Error()
		} else if errors.Is(walkErr, errRootTraversalPartial) {
			run.Status = "partial"
			run.ErrorMessage = walkErr.Error()
			// Parent checkpoints cannot be trusted after a child directory was
			// unreadable. Keep existing documents intact but force the retry to
			// enumerate the root again rather than silently skipping that child.
			if err := c.cat.ClearRootCheckpoints(ctx, root.ID); err != nil {
				run.Status = "failed"
				run.ErrorMessage = err.Error()
			}
		} else {
			run.Status = "failed"
			run.ErrorMessage = walkErr.Error()
		}
	} else {
		missing, err := c.cat.MarkMissing(ctx, root.ID, generation, c.snapshot().Crawler.MissingAfterSuccessfulCrawls)
		if err != nil {
			run.Status = "failed"
			run.ErrorMessage = err.Error()
		} else {
			run.FilesMissing = missing
			run.Status = "ok"
			if moved, moveErr := c.cat.ReconcileMoves(ctx, root.ID, generation); moveErr != nil {
				c.log.Warn("move reconciliation failed", "root", root.ID, "error", moveErr)
			} else if moved > 0 {
				c.log.Info("moves reconciled", "root", root.ID, "count", moved)
			}
		}
	}
	if err := c.finishCrawl(ctx, run); err != nil {
		return run, err
	}
	if run.Status == "unreachable" || run.Status == "partial" {
		c.logSuppressed("crawl-unreachable:"+root.ID, "crawl finished", run.ErrorMessage, 5*time.Minute, "root", root.ID, "status", run.Status, "seen", run.FilesSeen, "added", run.FilesAdded, "updated", run.FilesUpdated, "missing", run.FilesMissing, "errors", run.Errors, "error", run.ErrorMessage)
	} else {
		c.log.Info("crawl finished", "root", root.ID, "status", run.Status, "seen", run.FilesSeen, "added", run.FilesAdded, "updated", run.FilesUpdated, "missing", run.FilesMissing, "errors", run.Errors)
	}
	if run.Status == "partial" {
		return run, nil
	}
	return run, walkErr
}

func (c *Crawler) crawlHint(ctx context.Context, root config.RootConfig, path string, removed bool) (catalog.CrawlRun, error) {
	if atomic.LoadInt64(&c.maintenance) > 0 {
		return catalog.CrawlRun{RootID: root.ID, Status: "maintenance_paused", ErrorMessage: "crawler maintenance in progress"}, nil
	}
	var started bool
	ctx, finish, started := c.beginRootWork(ctx, root.ID)
	if !started {
		return catalog.CrawlRun{RootID: root.ID, Status: "already_running"}, nil
	}
	defer finish()
	atomic.AddInt64(&c.activeCrawls, 1)
	atomic.AddInt64(&c.hintCrawls, 1)
	atomic.StoreInt64(&c.lastActivity, time.Now().Unix())
	defer atomic.AddInt64(&c.activeCrawls, -1)
	generation, err := c.cat.NextGeneration(ctx, root.ID)
	if err != nil {
		return catalog.CrawlRun{}, err
	}
	run := catalog.CrawlRun{
		ID:         uuid.NewString(),
		RootID:     root.ID,
		Generation: generation,
		StartedAt:  time.Now().UTC(),
		Status:     "hint_running",
	}
	if err := c.cat.StartCrawl(ctx, run); err != nil {
		return run, err
	}
	if removed {
		missing, err := c.cat.MarkPathMissing(ctx, root.ID, path)
		run.FilesMissing = missing
		run.Status = "hint_ok"
		if err != nil {
			run.Status = "hint_failed"
			run.ErrorMessage = err.Error()
			run.Errors = 1
		}
		_ = c.finishCrawl(ctx, run)
		return run, err
	}

	info, err := os.Stat(path)
	if err != nil {
		run.Status = "hint_unreachable"
		run.ErrorMessage = err.Error()
		run.Errors = 1
		_ = c.finishCrawl(ctx, run)
		return run, nil
	}

	settings := c.snapshot().Crawler
	jobs := make(chan string, settings.MetadataQueueSize)
	var filesSeen, filesAdded, filesUpdated, filesUnchanged, errorsCount int64
	var workers sync.WaitGroup
	for i := 0; i < settings.MetadataWorkerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for p := range jobs {
				if c.waitIfPaused(ctx, true) != nil {
					return
				}
				res, err := c.indexPath(ctx, root, p, generation)
				if err != nil {
					if isCrawlerCancellation(ctx, err) {
						return
					}
					atomic.AddInt64(&errorsCount, 1)
					c.log.Warn("hint metadata index failed", "root", root.ID, "path", p, "error", err)
					continue
				}
				atomic.AddInt64(&filesSeen, 1)
				if res.Added {
					atomic.AddInt64(&filesAdded, 1)
				} else if res.Updated {
					atomic.AddInt64(&filesUpdated, 1)
				} else if res.Unchanged {
					atomic.AddInt64(&filesUnchanged, 1)
				}
			}
		}()
	}
	var walkErr error
	if info.IsDir() {
		walkErr = c.walkPaths(ctx, root, []string{path}, jobs, &errorsCount, generation, false, true)
	} else {
		walkErr = sendPath(ctx, jobs, path)
	}
	close(jobs)
	workers.Wait()

	run.FilesSeen = atomic.LoadInt64(&filesSeen)
	run.FilesAdded = atomic.LoadInt64(&filesAdded)
	run.FilesUpdated = atomic.LoadInt64(&filesUpdated)
	run.FilesUnchanged = atomic.LoadInt64(&filesUnchanged)
	run.Errors = atomic.LoadInt64(&errorsCount)
	if errors.Is(walkErr, context.Canceled) {
		run.Status = "cancelled"
	} else if walkErr != nil {
		run.Status = "hint_failed"
		run.ErrorMessage = walkErr.Error()
	} else {
		run.Status = "hint_ok"
	}
	if err := c.finishCrawl(ctx, run); err != nil {
		return run, err
	}
	c.log.Info("hint crawl finished", "root", root.ID, "path", path, "status", run.Status, "seen", run.FilesSeen, "added", run.FilesAdded, "updated", run.FilesUpdated, "missing", run.FilesMissing, "errors", run.Errors)
	return run, walkErr
}

func (c *Crawler) walkPaths(ctx context.Context, root config.RootConfig, paths []string, jobs chan<- string, errorsCount *int64, generation int64, resume bool, adaptivePause bool) error {
	queue := newDirQueue()
	go func() {
		<-ctx.Done()
		queue.finish()
	}()
	reachable := 0
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			atomic.AddInt64(errorsCount, 1)
			c.logSuppressed("root-stat:"+root.ID+":"+path, "root path stat failed", err.Error(), 5*time.Minute, "root", root.ID, "path", path, "error", err)
			continue
		}
		reachable++
		if info.IsDir() {
			if _, err := c.indexPath(ctx, root, path, generation); err != nil {
				if isCrawlerCancellation(ctx, err) {
					queue.finish()
					return ctx.Err()
				}
				atomic.AddInt64(errorsCount, 1)
				c.log.Warn("directory index failed", "root", root.ID, "path", path, "error", err)
			}
			queue.add(path)
			continue
		}
		if sendPath(ctx, jobs, path) != nil {
			queue.finish()
			return ctx.Err()
		}
	}
	if reachable == 0 {
		queue.finish()
		return fmt.Errorf("%w: no configured paths reachable for root %s", errRootTraversalIncomplete, root.ID)
	}

	workerCount := c.snapshot().Crawler.DirectoryWorkerCount
	if workerCount <= 0 {
		workerCount = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				dir, ok := queue.next(ctx)
				if !ok {
					return
				}
				if c.waitIfPaused(ctx, adaptivePause) != nil {
					queue.setErr(ctx.Err())
					queue.done()
					return
				}
				if resume {
					done, err := c.cat.DirectoryCheckpointed(ctx, root.ID, dir)
					if err != nil {
						if isCrawlerCancellation(ctx, err) {
							queue.setErr(ctx.Err())
							queue.done()
							return
						}
						atomic.AddInt64(errorsCount, 1)
						c.log.Warn("checkpoint read failed", "root", root.ID, "path", dir, "error", err)
					}
					if done {
						if err := c.cat.TouchPathGeneration(ctx, root.ID, dir, generation); err != nil {
							if isCrawlerCancellation(ctx, err) {
								queue.setErr(ctx.Err())
								queue.done()
								return
							}
							atomic.AddInt64(errorsCount, 1)
							c.log.Warn("checkpoint touch failed", "root", root.ID, "path", dir, "error", err)
						}
						queue.done()
						continue
					}
				}
				c.walkDirectory(ctx, root, dir, queue, jobs, errorsCount, generation, resume, adaptivePause)
				queue.done()
			}
		}()
	}
	wg.Wait()
	if err := queue.err(); err != nil {
		return err
	}
	return queue.partialErr()
}

func (c *Crawler) walkDirectory(ctx context.Context, root config.RootConfig, dir string, queue *dirQueue, jobs chan<- string, errorsCount *int64, generation int64, checkpoint bool, adaptivePause bool) {
	atomic.AddInt64(&c.dirsRead, 1)
	atomic.StoreInt64(&c.lastActivity, time.Now().Unix())
	entries, err := os.ReadDir(dir)
	if err != nil {
		atomic.AddInt64(errorsCount, 1)
		if ctx.Err() != nil {
			queue.setErr(ctx.Err())
			return
		}
		if isPermissionError(err) {
			c.logSuppressed("dir-access:"+root.ID+":"+dir, "directory access denied", err.Error(), 5*time.Minute, "root", root.ID, "path", dir, "error", err)
		} else {
			c.logSuppressed("dir-read:"+root.ID+":"+dir, "directory read failed", err.Error(), 5*time.Minute, "root", root.ID, "path", dir, "error", err)
		}
		queue.setPartialErr(fmt.Errorf("%w: could not read %s: %v", errRootTraversalPartial, dir, err))
		return
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			queue.setErr(ctx.Err())
			return
		}
		if c.waitIfPaused(ctx, adaptivePause) != nil {
			queue.setErr(ctx.Err())
			return
		}
		path := filepath.Join(dir, entry.Name())
		if shouldSkip(root, path, entry, c.snapshot().Crawler.IgnoreHidden) {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 && !c.snapshot().Crawler.FollowSymlinks {
			continue
		}
		if entry.IsDir() {
			if _, err := c.indexPath(ctx, root, path, generation); err != nil {
				if isCrawlerCancellation(ctx, err) {
					queue.setErr(ctx.Err())
					return
				}
				atomic.AddInt64(errorsCount, 1)
				c.log.Warn("directory index failed", "root", root.ID, "path", path, "error", err)
			}
			queue.add(path)
			continue
		}
		if err := sendPath(ctx, jobs, path); err != nil {
			queue.setErr(err)
			return
		}
	}
	if checkpoint {
		checkpointCtx, cancel := checkpointWriteContext(ctx)
		defer cancel()
		if err := c.cat.MarkDirectoryCheckpoint(checkpointCtx, root.ID, dir); err != nil {
			atomic.AddInt64(errorsCount, 1)
			c.log.Warn("checkpoint write failed", "root", root.ID, "path", dir, "error", err)
		} else if ctx.Err() != nil {
			c.log.Debug("checkpoint flushed after crawler stop", "root", root.ID, "path", dir)
		}
	}
}

func checkpointWriteContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func (c *Crawler) finishCrawl(ctx context.Context, run catalog.CrawlRun) error {
	finishCtx, cancel := checkpointWriteContext(ctx)
	defer cancel()
	return c.cat.FinishCrawl(finishCtx, run)
}

func isCrawlerCancellation(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (c *Crawler) waitIfPaused(ctx context.Context, adaptivePause bool) error {
	for {
		until := atomic.LoadInt64(&c.pausedUntil)
		if scheduledUntil, active := c.scheduledPauseUntil(time.Now()); active && scheduledUntil.Unix() > until {
			until = scheduledUntil.Unix()
		}
		now := time.Now().Unix()
		pressure := c.adaptiveStatus()
		if until <= now && (!adaptivePause || !pressure.Paused) {
			return ctx.Err()
		}
		wait := time.Second
		if (!adaptivePause || !pressure.Paused) && until-now < 1 {
			wait = time.Duration(until-now) * time.Second
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *Crawler) adaptiveMonitor(ctx context.Context) {
	self, selfErr := process.NewProcess(int32(os.Getpid()))
	if selfErr == nil {
		// Prime the process counter so each later reading covers the same window
		// as the system-wide CPU sample.
		_, _ = self.PercentWithContext(ctx, 0)
	}
	idleTicker := time.NewTicker(time.Second)
	defer idleTicker.Stop()
	for {
		settings := c.snapshot().Crawler.AdaptiveThrottle
		if !settings.Enabled {
			c.updateAdaptivePressure("", 0, 0, 1)
			select {
			case <-ctx.Done():
				return
			case <-idleTicker.C:
				continue
			}
		}
		// System counter collection is comparatively expensive on some platforms.
		// Only sample while indexing or enrichment work is actually in flight.
		if !c.hasActiveWork() {
			c.updateAdaptivePressure("", 0, 0, settings.RecoverySamples)
			select {
			case <-ctx.Done():
				return
			case <-idleTicker.C:
				continue
			}
		}

		previous, _ := disk.IOCountersWithContext(ctx)
		cpuPercent, err := cpu.PercentWithContext(ctx, settings.SampleInterval(), false)
		if err != nil || ctx.Err() != nil {
			return
		}
		current, _ := disk.IOCountersWithContext(ctx)
		diskBusy := diskBusyPercent(previous, current, settings.SampleInterval())
		previous = current
		cpuBusy := 0.0
		if len(cpuPercent) > 0 {
			cpuBusy = cpuPercent[0]
		}
		if selfErr == nil {
			if selfPercent, err := self.PercentWithContext(ctx, 0); err == nil {
				cpuBusy = externalCPUPercent(cpuBusy, selfPercent, runtime.NumCPU())
			}
		}
		reason := ""
		if cpuBusy >= settings.CPUPercentThreshold {
			reason = "cpu"
		}
		if diskBusy >= settings.DiskBusyPercentThreshold {
			if reason != "" {
				reason += "_and_disk"
			} else {
				reason = "disk"
			}
		}
		c.updateAdaptivePressure(reason, cpuBusy, diskBusy, settings.RecoverySamples)
	}
}

// externalCPUPercent removes this process's share from a system-wide CPU
// reading. Process CPU is reported as a percentage of a single logical core.
func externalCPUPercent(systemPercent, selfPercent float64, logicalCPUs int) float64 {
	if logicalCPUs < 1 {
		logicalCPUs = 1
	}
	external := systemPercent - selfPercent/float64(logicalCPUs)
	if external < 0 {
		return 0
	}
	return external
}

func (c *Crawler) hasActiveWork() bool {
	return atomic.LoadInt64(&c.activeCrawls) > 0 ||
		len(c.contentJobs) > 0 ||
		len(c.ocrJobs) > 0 ||
		len(c.hashJobs) > 0
}

func diskBusyPercent(previous, current map[string]disk.IOCountersStat, interval time.Duration) float64 {
	if interval <= 0 {
		return 0
	}
	var busiest float64
	for name, now := range current {
		before, ok := previous[name]
		if !ok || now.IoTime < before.IoTime {
			continue
		}
		busy := float64(now.IoTime-before.IoTime) / float64(interval.Milliseconds()) * 100
		if busy > 100 {
			busy = 100
		}
		if busy > busiest {
			busiest = busy
		}
	}
	return busiest
}

func (c *Crawler) updateAdaptivePressure(reason string, cpuPercent, diskBusy float64, recoverySamples int) {
	c.pressureMu.Lock()
	defer c.pressureMu.Unlock()
	c.pressure.CPUPercent = cpuPercent
	c.pressure.DiskBusyPercent = diskBusy
	if reason != "" {
		if !c.pressure.Paused {
			c.log.Info("crawler throttled by system pressure", "reason", reason, "cpu_percent", cpuPercent, "disk_busy_percent", diskBusy)
		}
		c.pressure.Paused = true
		c.pressure.Reason = reason
		c.pressure.HealthySamples = 0
		return
	}
	if !c.pressure.Paused {
		return
	}
	c.pressure.HealthySamples++
	if c.pressure.HealthySamples >= recoverySamples {
		c.log.Info("crawler resumed after system pressure", "cpu_percent", cpuPercent, "disk_busy_percent", diskBusy)
		c.pressure.Paused = false
		c.pressure.Reason = ""
		c.pressure.HealthySamples = 0
	}
}

func (c *Crawler) adaptiveStatus() adaptivePressure {
	c.pressureMu.RLock()
	defer c.pressureMu.RUnlock()
	return c.pressure
}

func (c *Crawler) scheduledPauseUntil(now time.Time) (time.Time, bool) {
	for _, window := range c.snapshot().Crawler.PauseWindows {
		if !windowApplies(now, window.Days) {
			continue
		}
		start, startErr := time.ParseInLocation("15:04", window.Start, now.Location())
		end, endErr := time.ParseInLocation("15:04", window.End, now.Location())
		if startErr != nil || endErr != nil || window.Start == window.End {
			continue
		}
		startAt := time.Date(now.Year(), now.Month(), now.Day(), start.Hour(), start.Minute(), 0, 0, now.Location())
		endAt := time.Date(now.Year(), now.Month(), now.Day(), end.Hour(), end.Minute(), 0, 0, now.Location())
		if endAt.Before(startAt) {
			endAt = endAt.Add(24 * time.Hour)
			if now.Before(startAt) {
				startAt = startAt.Add(-24 * time.Hour)
				endAt = endAt.Add(-24 * time.Hour)
			}
		}
		if !now.Before(startAt) && now.Before(endAt) {
			return endAt, true
		}
	}
	return time.Time{}, false
}

func windowApplies(now time.Time, days []string) bool {
	if len(days) == 0 {
		return true
	}
	day := strings.ToLower(now.Weekday().String()[:3])
	for _, candidate := range days {
		if strings.ToLower(strings.TrimSpace(candidate)) == day {
			return true
		}
	}
	return false
}

func sendPath(ctx context.Context, jobs chan<- string, path string) error {
	select {
	case jobs <- path:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type dirQueue struct {
	mu         sync.Mutex
	cond       *sync.Cond
	dirs       []string
	pending    int
	errVal     error
	partialVal error
}

func newDirQueue() *dirQueue {
	q := &dirQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *dirQueue) add(path string) {
	q.mu.Lock()
	q.dirs = append(q.dirs, path)
	q.pending++
	q.cond.Signal()
	q.mu.Unlock()
}

func (q *dirQueue) next(ctx context.Context) (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.dirs) == 0 && q.pending > 0 && q.errVal == nil && ctx.Err() == nil {
		q.cond.Wait()
	}
	if q.errVal != nil || ctx.Err() != nil || len(q.dirs) == 0 {
		return "", false
	}
	dir := q.dirs[0]
	copy(q.dirs, q.dirs[1:])
	q.dirs = q.dirs[:len(q.dirs)-1]
	return dir, true
}

func (q *dirQueue) done() {
	q.mu.Lock()
	q.pending--
	if q.pending <= 0 {
		q.cond.Broadcast()
	}
	q.mu.Unlock()
}

func (q *dirQueue) finish() {
	q.mu.Lock()
	q.pending = 0
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *dirQueue) setErr(err error) {
	if err == nil {
		return
	}
	q.mu.Lock()
	if q.errVal == nil {
		q.errVal = err
	}
	q.cond.Broadcast()
	q.mu.Unlock()
}

func (q *dirQueue) err() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.errVal
}

func (q *dirQueue) setPartialErr(err error) {
	if err == nil {
		return
	}
	q.mu.Lock()
	if q.partialVal == nil {
		q.partialVal = err
	}
	q.mu.Unlock()
}

func (q *dirQueue) partialErr() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.partialVal
}

func ExpandRootPaths(path string) ([]string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, os.ErrNotExist
	}
	if parent, ok := simpleChildrenGlobParent(path); ok {
		entries, err := os.ReadDir(parent)
		if err != nil {
			return nil, err
		}
		paths := make([]string, 0, len(entries))
		for _, entry := range entries {
			paths = append(paths, filepath.Join(parent, entry.Name()))
		}
		if len(paths) == 0 {
			return nil, os.ErrNotExist
		}
		return paths, nil
	}
	if hasGlob(path) {
		matches, err := filepath.Glob(path)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			return nil, os.ErrNotExist
		}
		return matches, nil
	}
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return []string{path}, nil
}

func simpleChildrenGlobParent(path string) (string, bool) {
	cleaned := filepath.Clean(path)
	if !strings.HasSuffix(cleaned, string(filepath.Separator)+"*") {
		return "", false
	}
	parent := strings.TrimSuffix(cleaned, string(filepath.Separator)+"*")
	if parent == "" {
		return "", false
	}
	if runtime.GOOS == "windows" && len(parent) == 2 && parent[1] == ':' {
		parent += string(filepath.Separator)
	}
	if _, err := os.Stat(parent); err != nil {
		return parent, false
	}
	return parent, true
}

func hasGlob(path string) bool {
	return strings.ContainsAny(path, "*?[")
}

func (c *Crawler) indexPath(ctx context.Context, root config.RootConfig, path string, generation int64) (catalog.UpsertResult, error) {
	doc, indexable, err := c.describePath(root, path, generation)
	if err != nil {
		return catalog.UpsertResult{}, err
	}
	if !indexable {
		return catalog.UpsertResult{Unchanged: true}, nil
	}
	return c.cat.UpsertDocument(ctx, doc)
}

// describePath performs filesystem work only. Full crawls batch the resulting
// catalog documents; hints and directory records still use indexPath directly.
func (c *Crawler) describePath(root config.RootConfig, path string, generation int64) (catalog.Document, bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return catalog.Document{}, false, err
	}
	isFolder := info.IsDir()
	if !isFolder {
		atomic.AddInt64(&c.filesStatted, 1)
		atomic.AddInt64(&c.bytesStatted, info.Size())
	}
	atomic.StoreInt64(&c.lastActivity, time.Now().Unix())
	ext := ""
	if !isFolder {
		ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
		if !extensionAllowed(root, ext) {
			return catalog.Document{}, false, nil
		}
		if !filePatternAllowed(root, path) {
			return catalog.Document{}, false, nil
		}
	}
	doc := catalog.Document{
		ID:                 uuid.NewString(),
		RootID:             root.ID,
		Path:               path,
		NormalizedPath:     catalog.NormalizePath(path),
		Name:               filepath.Base(path),
		Extension:          ext,
		IsFolder:           isFolder,
		Size:               info.Size(),
		ModifiedAt:         info.ModTime(),
		LastSeenGeneration: generation,
		Signature:          catalog.Signature(info.Size(), info.ModTime()),
		AccessStatus:       "metadata_readable",
	}
	if root.OwnershipEnabled(c.snapshot().Crawler.CollectOwnership) {
		doc.Owner = filemeta.Owner(path)
	}
	return doc, true, nil
}

func isPermissionError(err error) bool {
	return errors.Is(err, fs.ErrPermission) || os.IsPermission(err) || strings.Contains(strings.ToLower(err.Error()), "access denied") || strings.Contains(strings.ToLower(err.Error()), "permission denied")
}

func (c *Crawler) startBackgroundWorkers(ctx context.Context, workers *sync.WaitGroup) {
	for i := 0; i < c.snapshot().Crawler.ContentExtraction.WorkerCount; i++ {
		startCrawlerRoutine(ctx, workers, c.log, c.contentWorker)
	}
	for i := 0; i < c.snapshot().Crawler.OCR.WorkerCount; i++ {
		startCrawlerRoutine(ctx, workers, c.log, c.ocrWorker)
	}
	for i := 0; i < c.snapshot().Crawler.Hashing.WorkerCount; i++ {
		startCrawlerRoutine(ctx, workers, c.log, c.hashWorker)
	}
}

func (c *Crawler) contentWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-c.contentJobs:
			if ctx.Err() != nil {
				return
			}
			if c.waitForEnrichmentWindow(ctx) != nil {
				return
			}
			jobCtx, finish := c.beginBackgroundWork(ctx)
			root, exists := c.rootByID(job.rootID)
			if !exists || !root.ContentExtractionEnabled(c.snapshot().Crawler.ContentExtraction.Enabled) {
				if jobCtx.Err() == nil {
					_ = c.cat.UpdateContentStatus(jobCtx, job.id, job.signature, "not_indexed")
				}
				finish()
				continue
			}
			text, err := extract.Text(job.path)
			if jobCtx.Err() != nil {
				finish()
				continue
			}
			status := "extracted"
			if err != nil {
				status = "failed"
				text = ""
				c.log.Debug("content extraction failed", "path", job.path, "error", err)
			}
			if err := c.cat.UpdateExtractedContent(jobCtx, job.id, job.signature, status, text); err != nil {
				c.log.Debug("content update failed", "path", job.path, "error", err)
				if releaseErr := c.cat.ReleaseContentClaim(context.Background(), job.id, job.signature); releaseErr != nil {
					c.log.Debug("content claim release failed", "path", job.path, "error", releaseErr)
				}
				finish()
				continue
			}
			if err == nil && exists && strings.TrimSpace(text) == "" && root.OCREnabled(c.snapshot().Crawler.OCR.Enabled) && extract.OCREligible(job.path) {
				if updateErr := c.cat.UpdateOCRStatus(jobCtx, job.id, job.signature, "pending"); updateErr != nil {
					c.log.Debug("OCR pending update failed", "path", job.path, "error", updateErr)
				}
			}
			finish()
		}
	}
}

func (c *Crawler) ocrWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-c.ocrJobs:
			if ctx.Err() != nil {
				return
			}
			if c.waitForEnrichmentWindow(ctx) != nil {
				return
			}
			jobCtx, finish := c.beginBackgroundWork(ctx)
			settings := c.snapshot().Crawler.OCR
			root, exists := c.rootByID(job.rootID)
			if !exists || !root.ContentExtractionEnabled(c.snapshot().Crawler.ContentExtraction.Enabled) || !root.OCREnabled(settings.Enabled) {
				if jobCtx.Err() == nil {
					_ = c.cat.UpdateOCRStatus(jobCtx, job.id, job.signature, "pending")
				}
				finish()
				continue
			}
			timedCtx, cancel := context.WithTimeout(jobCtx, time.Duration(settings.TimeoutSeconds)*time.Second)
			status, text, err := extract.OCR(timedCtx, job.path, extract.OCROptions{
				Engine: settings.Engine, TesseractCommand: settings.TesseractCommand,
				OCRmyPDFCommand: settings.OCRmyPDFCommand, Languages: settings.Languages,
			})
			cancel()
			if jobCtx.Err() != nil {
				finish()
				continue
			}
			if err != nil {
				c.log.Debug("OCR failed", "path", job.path, "engine", settings.Engine, "error", err)
			}
			if err := c.cat.UpdateOCRContent(jobCtx, job.id, job.signature, status, text); err != nil {
				c.log.Debug("OCR update failed", "path", job.path, "error", err)
				if releaseErr := c.cat.ReleaseOCRClaim(context.Background(), job.id, job.signature); releaseErr != nil {
					c.log.Debug("OCR claim release failed", "path", job.path, "error", releaseErr)
				}
			}
			finish()
		}
	}
}

func (c *Crawler) hashWorker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-c.hashJobs:
			if ctx.Err() != nil {
				return
			}
			if c.waitForEnrichmentWindow(ctx) != nil {
				return
			}
			jobCtx, finish := c.beginBackgroundWork(ctx)
			root, exists := c.rootByID(job.rootID)
			if !exists || !root.HashingEnabled(c.snapshot().Crawler.Hashing.Enabled) {
				if jobCtx.Err() == nil {
					_ = c.cat.UpdateContentHash(jobCtx, job.id, job.signature, "", "not_hashed")
				}
				finish()
				continue
			}
			f, err := os.Open(job.path)
			status, value := "hashed", ""
			if err == nil {
				h := sha256.New()
				_, err = io.Copy(h, f)
				_ = f.Close()
				if err == nil {
					value = fmt.Sprintf("sha256:%x", h.Sum(nil))
				}
			}
			if err != nil {
				status = "failed"
			}
			if jobCtx.Err() != nil {
				finish()
				continue
			}
			if err := c.cat.UpdateContentHash(jobCtx, job.id, job.signature, value, status); err != nil {
				c.log.Debug("hash update failed", "path", job.path, "error", err)
				if releaseErr := c.cat.ReleaseHashClaim(context.Background(), job.id, job.signature); releaseErr != nil {
					c.log.Debug("hash claim release failed", "path", job.path, "error", releaseErr)
				}
			}
			finish()
		}
	}
}

func (c *Crawler) rootByID(rootID string) (config.RootConfig, bool) {
	for _, root := range c.snapshot().Roots {
		if root.ID == rootID {
			return root, true
		}
	}
	return config.RootConfig{}, false
}

func (c *Crawler) rootIDsFor(include func(config.RootConfig) bool) []string {
	roots := c.snapshot().Roots
	rootIDs := make([]string, 0, len(roots))
	for _, root := range roots {
		if root.Enabled && include(root) {
			rootIDs = append(rootIDs, root.ID)
		}
	}
	return rootIDs
}

func shouldSkip(root config.RootConfig, path string, entry fs.DirEntry, ignoreHidden bool) bool {
	name := entry.Name()
	if ignoreHidden && strings.HasPrefix(name, ".") {
		return true
	}
	if entry.IsDir() && (matchesAny(path, root.ExcludeFolderPatterns) || matchesAny(path, []string{"**/@Recently-Snapshot/**", "**/@Recycle/**", "**/#recycle/**", "**/$RECYCLE.BIN/**", "**/RECYCLER/**", "**/.sync/**", "**/.qsync/**", "**/.qsync_sn/**"})) {
		return true
	}
	return matchesAny(path, root.ExcludePatterns)
}

func extensionAllowed(root config.RootConfig, ext string) bool {
	if len(root.IncludeExtensions) > 0 && !containsExt(root.IncludeExtensions, ext) && !containsExt(root.IncludeExtensions, "*") {
		return false
	}
	return !containsExt(root.ExcludeExtensions, ext)
}

func containsExt(candidates []string, ext string) bool {
	for _, candidate := range candidates {
		if strings.TrimPrefix(strings.ToLower(candidate), ".") == ext || strings.TrimSpace(candidate) == "*" {
			return true
		}
	}
	return false
}

func filePatternAllowed(root config.RootConfig, path string) bool {
	if len(root.IncludeFolderPatterns) > 0 && !matchesAny(filepath.Dir(path), root.IncludeFolderPatterns) {
		return false
	}
	if len(root.IncludeFilePatterns) > 0 && !matchesAny(path, root.IncludeFilePatterns) {
		return false
	}
	if matchesAny(filepath.Dir(path), root.ExcludeFolderPatterns) {
		return false
	}
	return !matchesAny(path, root.ExcludeFilePatterns)
}

func matchesAny(path string, patterns []string) bool {
	normalized := comparablePath(path)
	base := comparablePath(filepath.Base(path))
	for _, pattern := range patterns {
		p := comparablePath(strings.TrimSpace(pattern))
		if p == "" {
			continue
		}
		if pathGlobMatch(p, normalized) {
			return true
		}
		if pathGlobMatch(p, base) {
			return true
		}
		if strings.HasPrefix(p, "**/") {
			needle := strings.TrimPrefix(p, "**/")
			if pathGlobMatch(needle, base) {
				return true
			}
		}
		if strings.HasSuffix(p, "/**") {
			needle := strings.TrimSuffix(p, "/**")
			needle = strings.TrimPrefix(needle, "**/")
			if needle != "" && (strings.Contains(normalized, "/"+needle+"/") || strings.HasSuffix(normalized, "/"+needle)) {
				return true
			}
		}
	}
	return false
}

func comparablePath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.ReplaceAll(path, `\`, "/")
	for strings.Contains(path, "//") && !strings.HasPrefix(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	if runtime.GOOS == "windows" || looksLikeWindowsPath(path) {
		path = strings.ToLower(path)
	}
	return path
}

func pathGlobMatch(pattern, path string) bool {
	if ok, _ := pathmatch.Match(pattern, path); ok {
		return true
	}
	if !strings.Contains(pattern, "**") {
		return false
	}
	return doubleStarMatch(pattern, path)
}

func doubleStarMatch(pattern, path string) bool {
	parts := strings.Split(pattern, "**")
	if len(parts) == 1 {
		return pattern == path
	}
	pos := 0
	if parts[0] != "" {
		if !strings.HasPrefix(path, parts[0]) {
			return false
		}
		pos = len(parts[0])
	}
	for _, part := range parts[1:] {
		if part == "" {
			continue
		}
		idx := strings.Index(path[pos:], part)
		if idx < 0 {
			return false
		}
		pos += idx + len(part)
	}
	last := parts[len(parts)-1]
	return last == "" || strings.HasSuffix(path, last)
}

func looksLikeWindowsPath(path string) bool {
	if len(path) >= 3 && path[1] == ':' && path[2] == '/' {
		return true
	}
	return strings.HasPrefix(path, "//")
}

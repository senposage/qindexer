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

type Crawler struct {
	cfg           *config.Config
	cat           *catalog.Catalog
	log           *slog.Logger
	mu            sync.Mutex
	run           map[string]context.CancelFunc
	done          map[string]chan struct{}
	startedAt     time.Time
	activeCrawls  int64
	filesStatted  int64
	bytesStatted  int64
	dirsRead      int64
	hintCrawls    int64
	fullCrawls    int64
	initialCrawls int64
	lastActivity  int64
	pausedUntil   int64
	pressureMu    sync.RWMutex
	pressure      adaptivePressure
	contentJobs   chan backgroundJob
	ocrJobs       chan backgroundJob
	hashJobs      chan backgroundJob
}

type backgroundJob struct {
	id, rootID, path, signature string
	size                        int64
}

type adaptivePressure struct {
	Paused          bool
	Reason          string
	CPUPercent      float64
	DiskBusyPercent float64
	HealthySamples  int
}

func New(cfg *config.Config, cat *catalog.Catalog, log *slog.Logger) *Crawler {
	return &Crawler{cfg: cfg, cat: cat, log: log, run: map[string]context.CancelFunc{}, done: map[string]chan struct{}{}, startedAt: time.Now().UTC(), contentJobs: make(chan backgroundJob, cfg.Crawler.ContentExtraction.QueueSize), ocrJobs: make(chan backgroundJob, cfg.Crawler.OCR.QueueSize), hashJobs: make(chan backgroundJob, cfg.Crawler.Hashing.QueueSize)}
}

type Stats struct {
	StartedAt        time.Time `json:"started_at"`
	UptimeSeconds    float64   `json:"uptime_seconds"`
	ActiveCrawls     int64     `json:"active_crawls"`
	InitialCrawls    int64     `json:"initial_crawls"`
	FilesStatted     int64     `json:"files_statted"`
	DirectoriesRead  int64     `json:"directories_read"`
	BytesStatted     int64     `json:"bytes_statted"`
	FilesPerSecond   float64   `json:"files_per_second"`
	DirsPerSecond    float64   `json:"directories_per_second"`
	BytesPerSecond   float64   `json:"bytes_per_second"`
	HintCrawls       int64     `json:"hint_crawls"`
	FullCrawls       int64     `json:"full_crawls"`
	LastActivityUnix int64     `json:"last_activity_unix"`
	Paused           bool      `json:"paused"`
	PausedUntilUnix  int64     `json:"paused_until_unix"`
	PauseReason      string    `json:"pause_reason,omitempty"`
	AdaptivePaused   bool      `json:"adaptive_paused"`
	CPUPercent       float64   `json:"cpu_percent"`
	DiskBusyPercent  float64   `json:"disk_busy_percent"`
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
		FilesStatted: files, DirectoriesRead: dirs, BytesStatted: bytes,
		FilesPerSecond: float64(files) / uptime, DirsPerSecond: float64(dirs) / uptime,
		BytesPerSecond: float64(bytes) / uptime,
		HintCrawls:     atomic.LoadInt64(&c.hintCrawls), FullCrawls: atomic.LoadInt64(&c.fullCrawls),
		LastActivityUnix: atomic.LoadInt64(&c.lastActivity),
		Paused:           pausedUntil > time.Now().Unix() || (pressure.Paused && initialCrawls == 0),
		PausedUntilUnix:  pausedUntil,
		PauseReason:      pausedReason,
		AdaptivePaused:   pressure.Paused,
		CPUPercent:       pressure.CPUPercent,
		DiskBusyPercent:  pressure.DiskBusyPercent,
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

func (c *Crawler) CancelRoot(rootID string) bool {
	c.mu.Lock()
	cancel := c.run[rootID]
	c.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

func (c *Crawler) CancelAll() {
	c.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(c.run))
	for _, cancel := range c.run {
		cancels = append(cancels, cancel)
	}
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
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
	return true
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
	sem := make(chan struct{}, c.cfg.Crawler.RootParallelism)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var runs []catalog.CrawlRun
	for _, root := range c.cfg.Roots {
		if !root.Enabled || !include(root) {
			continue
		}
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
	wg.Wait()
	return runs
}

func (c *Crawler) Loop(ctx context.Context) {
	var workers sync.WaitGroup
	defer workers.Wait()
	startCrawlerRoutine(ctx, &workers, c.adaptiveMonitor)
	c.startBackgroundWorkers(ctx, &workers)
	startCrawlerRoutine(ctx, &workers, c.refillBackgroundLoop)
	ticker := time.NewTicker(c.cfg.Crawler.ScanInterval())
	defer ticker.Stop()
	if c.waitIfPaused(ctx, true) != nil {
		return
	}
	// A restart should preserve existing index state. Only new or incomplete
	// roots need an initial crawl; watcher events cover ordinary changes.
	c.CrawlUnindexedRoots(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if c.waitIfPaused(ctx, true) != nil {
				return
			}
			c.CrawlAll(ctx)
		}
	}
}

func startCrawlerRoutine(ctx context.Context, workers *sync.WaitGroup, fn func(context.Context)) {
	workers.Add(1)
	go func() {
		defer workers.Done()
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
	content := c.cfg.Crawler.ContentExtraction
	if rootIDs := c.rootIDsFor(func(root config.RootConfig) bool { return root.ContentExtractionEnabled(content.Enabled) }); len(rootIDs) > 0 {
		c.fillBackground(ctx, rootIDs, c.contentJobs, content.MaxFileSizeMB<<20, c.cat.ClaimPendingContent, "content")
	}
	ocr := c.cfg.Crawler.OCR
	if rootIDs := c.rootIDsFor(func(root config.RootConfig) bool {
		return root.ContentExtractionEnabled(content.Enabled) && root.OCREnabled(ocr.Enabled)
	}); len(rootIDs) > 0 {
		c.fillBackground(ctx, rootIDs, c.ocrJobs, ocr.MaxFileSizeMB<<20, c.cat.ClaimPendingOCR, "OCR")
	}
	hashing := c.cfg.Crawler.Hashing
	if rootIDs := c.rootIDsFor(func(root config.RootConfig) bool { return root.HashingEnabled(hashing.Enabled) }); len(rootIDs) > 0 {
		c.fillBackground(ctx, rootIDs, c.hashJobs, hashing.MaxFileSizeMB<<20, c.cat.ClaimPendingHashes, "hash")
	}
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	c.mu.Lock()
	if c.run[root.ID] != nil {
		c.mu.Unlock()
		return catalog.CrawlRun{RootID: root.ID, Status: "already_running"}, nil
	}
	c.run[root.ID] = cancel
	c.done[root.ID] = done
	c.mu.Unlock()
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
		c.mu.Lock()
		delete(c.run, root.ID)
		delete(c.done, root.ID)
		close(done)
		c.mu.Unlock()
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
		_ = c.cat.FinishCrawl(ctx, run)
		return run, err
	}

	jobs := make(chan string, c.cfg.Crawler.MetadataQueueSize)
	var filesSeen, filesAdded, filesUpdated, filesUnchanged, errorsCount int64
	var workers sync.WaitGroup
	for i := 0; i < c.cfg.Crawler.MetadataWorkerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for path := range jobs {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if c.waitIfPaused(ctx, !initial) != nil {
					return
				}
				res, err := c.indexPath(ctx, root, path, generation)
				if err != nil {
					atomic.AddInt64(&errorsCount, 1)
					c.log.Warn("metadata index failed", "root", root.ID, "path", path, "error", err)
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

	walkErr := c.walkPaths(ctx, root, paths, jobs, &errorsCount, generation, true, !initial)
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
		} else {
			run.Status = "failed"
			run.ErrorMessage = walkErr.Error()
		}
	} else {
		missing, err := c.cat.MarkMissing(ctx, root.ID, generation, c.cfg.Crawler.MissingAfterSuccessfulCrawls)
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
	finishCtx := ctx
	if ctx.Err() != nil {
		var finishCancel context.CancelFunc
		finishCtx, finishCancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer finishCancel()
	}
	if err := c.cat.FinishCrawl(finishCtx, run); err != nil {
		return run, err
	}
	c.log.Info("crawl finished", "root", root.ID, "status", run.Status, "seen", run.FilesSeen, "added", run.FilesAdded, "updated", run.FilesUpdated, "missing", run.FilesMissing, "errors", run.Errors)
	return run, walkErr
}

func (c *Crawler) crawlHint(ctx context.Context, root config.RootConfig, path string, removed bool) (catalog.CrawlRun, error) {
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
		_ = c.cat.FinishCrawl(ctx, run)
		return run, err
	}

	info, err := os.Stat(path)
	if err != nil {
		missing, markErr := c.cat.MarkPathMissing(ctx, root.ID, path)
		run.FilesMissing = missing
		run.Status = "hint_ok"
		if markErr != nil {
			run.Status = "hint_failed"
			run.ErrorMessage = markErr.Error()
			run.Errors = 1
		}
		_ = c.cat.FinishCrawl(ctx, run)
		return run, markErr
	}

	jobs := make(chan string, c.cfg.Crawler.MetadataQueueSize)
	var filesSeen, filesAdded, filesUpdated, filesUnchanged, errorsCount int64
	var workers sync.WaitGroup
	for i := 0; i < c.cfg.Crawler.MetadataWorkerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for p := range jobs {
				if c.waitIfPaused(ctx, true) != nil {
					return
				}
				res, err := c.indexPath(ctx, root, p, generation)
				if err != nil {
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
	if walkErr != nil {
		run.Status = "hint_failed"
		run.ErrorMessage = walkErr.Error()
	} else {
		run.Status = "hint_ok"
	}
	if err := c.cat.FinishCrawl(ctx, run); err != nil {
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
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil {
			c.recordPathFailure(ctx, root, path, err, generation)
			atomic.AddInt64(errorsCount, 1)
			c.log.Warn("root path stat failed", "root", root.ID, "path", path, "error", err)
			continue
		}
		if info.IsDir() {
			if _, err := c.indexPath(ctx, root, path, generation); err != nil {
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

	workerCount := c.cfg.Crawler.DirectoryWorkerCount
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
						atomic.AddInt64(errorsCount, 1)
						c.log.Warn("checkpoint read failed", "root", root.ID, "path", dir, "error", err)
					}
					if done {
						if err := c.cat.TouchPathGeneration(ctx, root.ID, dir, generation); err != nil {
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
	return queue.err()
}

func (c *Crawler) walkDirectory(ctx context.Context, root config.RootConfig, dir string, queue *dirQueue, jobs chan<- string, errorsCount *int64, generation int64, checkpoint bool, adaptivePause bool) {
	atomic.AddInt64(&c.dirsRead, 1)
	atomic.StoreInt64(&c.lastActivity, time.Now().Unix())
	entries, err := os.ReadDir(dir)
	if err != nil {
		c.recordPathFailure(ctx, root, dir, err, generation)
		atomic.AddInt64(errorsCount, 1)
		c.log.Warn("directory read failed", "root", root.ID, "path", dir, "error", err)
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
		if shouldSkip(root, path, entry, c.cfg.Crawler.IgnoreHidden) {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 && !c.cfg.Crawler.FollowSymlinks {
			continue
		}
		if entry.IsDir() {
			if _, err := c.indexPath(ctx, root, path, generation); err != nil {
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
		if err := c.cat.MarkDirectoryCheckpoint(ctx, root.ID, dir); err != nil {
			atomic.AddInt64(errorsCount, 1)
			c.log.Warn("checkpoint write failed", "root", root.ID, "path", dir, "error", err)
		}
	}
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
	settings := c.cfg.Crawler.AdaptiveThrottle
	if !settings.Enabled {
		return
	}
	self, selfErr := process.NewProcess(int32(os.Getpid()))
	if selfErr == nil {
		// Prime the process counter so each later reading covers the same window
		// as the system-wide CPU sample.
		_, _ = self.PercentWithContext(ctx, 0)
	}
	idleTicker := time.NewTicker(time.Second)
	defer idleTicker.Stop()
	for {
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
	for _, window := range c.cfg.Crawler.PauseWindows {
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
	mu      sync.Mutex
	cond    *sync.Cond
	dirs    []string
	pending int
	errVal  error
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
	info, err := os.Stat(path)
	if err != nil {
		c.recordPathFailure(ctx, root, path, err, generation)
		return catalog.UpsertResult{}, err
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
			return catalog.UpsertResult{Unchanged: true}, nil
		}
		if !filePatternAllowed(root, path) {
			return catalog.UpsertResult{Unchanged: true}, nil
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
	if root.OwnershipEnabled(c.cfg.Crawler.CollectOwnership) {
		doc.Owner = filemeta.Owner(path)
	}
	return c.cat.UpsertDocument(ctx, doc)
}

func (c *Crawler) recordPathFailure(ctx context.Context, root config.RootConfig, path string, err error, generation int64) {
	if os.IsNotExist(err) {
		_, _ = c.cat.MarkPathMissing(ctx, root.ID, path)
		return
	}
	_, _ = c.cat.MarkPathInaccessible(ctx, root.ID, path, generation)
}

func (c *Crawler) startBackgroundWorkers(ctx context.Context, workers *sync.WaitGroup) {
	for i := 0; i < c.cfg.Crawler.ContentExtraction.WorkerCount; i++ {
		startCrawlerRoutine(ctx, workers, c.contentWorker)
	}
	for i := 0; i < c.cfg.Crawler.OCR.WorkerCount; i++ {
		startCrawlerRoutine(ctx, workers, c.ocrWorker)
	}
	for i := 0; i < c.cfg.Crawler.Hashing.WorkerCount; i++ {
		startCrawlerRoutine(ctx, workers, c.hashWorker)
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
			if c.waitIfPaused(ctx, true) != nil {
				return
			}
			root, exists := c.rootByID(job.rootID)
			if !exists || !root.ContentExtractionEnabled(c.cfg.Crawler.ContentExtraction.Enabled) {
				_ = c.cat.UpdateContentStatus(ctx, job.id, job.signature, "not_indexed")
				continue
			}
			text, err := extract.Text(job.path)
			status := "extracted"
			if err != nil {
				status = "failed"
				text = ""
			}
			if err := c.cat.UpdateExtractedContent(ctx, job.id, job.signature, status, text); err != nil {
				c.log.Debug("content update failed", "path", job.path, "error", err)
				continue
			}
			if err == nil && exists && strings.TrimSpace(text) == "" && root.OCREnabled(c.cfg.Crawler.OCR.Enabled) && extract.OCREligible(job.path) {
				if updateErr := c.cat.UpdateOCRStatus(ctx, job.id, job.signature, "pending"); updateErr != nil {
					c.log.Debug("OCR pending update failed", "path", job.path, "error", updateErr)
				}
			}
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
			if c.waitIfPaused(ctx, true) != nil {
				return
			}
			settings := c.cfg.Crawler.OCR
			root, exists := c.rootByID(job.rootID)
			if !exists || !root.ContentExtractionEnabled(c.cfg.Crawler.ContentExtraction.Enabled) || !root.OCREnabled(settings.Enabled) {
				_ = c.cat.UpdateOCRStatus(ctx, job.id, job.signature, "pending")
				continue
			}
			jobCtx, cancel := context.WithTimeout(ctx, time.Duration(settings.TimeoutSeconds)*time.Second)
			status, text, err := extract.OCR(jobCtx, job.path, extract.OCROptions{
				Engine: settings.Engine, TesseractCommand: settings.TesseractCommand,
				OCRmyPDFCommand: settings.OCRmyPDFCommand, Languages: settings.Languages,
			})
			cancel()
			if err != nil {
				c.log.Debug("OCR failed", "path", job.path, "engine", settings.Engine, "error", err)
			}
			if err := c.cat.UpdateOCRContent(ctx, job.id, job.signature, status, text); err != nil {
				c.log.Debug("OCR update failed", "path", job.path, "error", err)
			}
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
			if c.waitIfPaused(ctx, true) != nil {
				return
			}
			root, exists := c.rootByID(job.rootID)
			if !exists || !root.HashingEnabled(c.cfg.Crawler.Hashing.Enabled) {
				_ = c.cat.UpdateContentHash(ctx, job.id, job.signature, "", "not_hashed")
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
			if err := c.cat.UpdateContentHash(ctx, job.id, job.signature, value, status); err != nil {
				c.log.Debug("hash update failed", "path", job.path, "error", err)
			}
		}
	}
}

func (c *Crawler) rootByID(rootID string) (config.RootConfig, bool) {
	for _, root := range c.cfg.Roots {
		if root.ID == rootID {
			return root, true
		}
	}
	return config.RootConfig{}, false
}

func (c *Crawler) rootIDsFor(include func(config.RootConfig) bool) []string {
	rootIDs := make([]string, 0, len(c.cfg.Roots))
	for _, root := range c.cfg.Roots {
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

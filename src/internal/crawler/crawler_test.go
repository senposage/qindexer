package crawler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shirou/gopsutil/v4/disk"

	"qindexer/internal/catalog"
	"qindexer/internal/config"
)

func TestExpandRootPathsSupportsGlob(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")
	if err := os.Mkdir(first, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(second, 0755); err != nil {
		t.Fatal(err)
	}

	paths, err := ExpandRootPaths(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %d: %v", len(paths), paths)
	}
}

func TestExpandRootPathsRejectsEmptyGlob(t *testing.T) {
	_, err := ExpandRootPaths(filepath.Join(t.TempDir(), "*"))
	if err == nil {
		t.Fatal("expected empty glob error")
	}
}

func TestExtensionPoliciesMatchDottedAndLegacyEntries(t *testing.T) {
	if !extensionAllowed(config.RootConfig{IncludeExtensions: []string{".xml"}}, "xml") {
		t.Fatal("dotted include policy should match a filename extension")
	}
	if !extensionAllowed(config.RootConfig{IncludeExtensions: []string{"h"}}, ".h") {
		t.Fatal("legacy bare include policy should remain compatible")
	}
	if !extensionExcluded("xml", []string{".xml"}) {
		t.Fatal("dotted exclusion should match a filename extension")
	}
	if !extensionExcluded(".h", []string{"h"}) {
		t.Fatal("legacy bare exclusion should remain compatible")
	}
}

func TestEnrichmentFailureMessageDistinguishesFileAndEngineFailures(t *testing.T) {
	if got := enrichmentFailureMessage("OCR", "unindexable", errors.New(`OCR command "tesseract" failed`)); got != "file not eligible for ocr" {
		t.Fatalf("terminal OCR failure message = %q", got)
	}
	if got := enrichmentFailureMessage("Content extraction", "retry_pending", errors.New("resource temporarily unavailable")); got != "Content extraction deferred; file will be retried" {
		t.Fatalf("retryable content failure message = %q", got)
	}
	if got := enrichmentFailureMessage("OCR", "unindexable", errors.New(`OCR command "tesseract" was not found`)); got != "OCR engine unavailable" {
		t.Fatalf("missing engine message = %q", got)
	}
}

func TestExcludedImageIsNominatedForOCRWithoutContentExtraction(t *testing.T) {
	rootPath := t.TempDir()
	path := filepath.Join(rootPath, "scan.jpg")
	if err := os.WriteFile(path, []byte("image"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Crawler: config.CrawlerConfig{
		ContentExtraction: config.ContentExtractionConfig{Enabled: true, ExcludeExtensions: []string{"jpg"}},
		OCR:               config.OCRConfig{Enabled: true},
	}}
	c := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	doc, indexable, err := c.describePath(config.RootConfig{ID: "root", Path: rootPath, Enabled: true}, path, 1)
	if err != nil || !indexable {
		t.Fatalf("describe image = %#v, indexable=%t, err=%v", doc, indexable, err)
	}
	if doc.OCRStatus != "pending" || doc.ContentStatus != "" {
		t.Fatalf("excluded image routing = %#v", doc)
	}
}

func TestRootOverrideSupersedesGlobalEnrichmentExclusions(t *testing.T) {
	empty := []string{}
	cfg := &config.Config{Crawler: config.CrawlerConfig{
		ContentExtraction: config.ContentExtractionConfig{ExcludeExtensions: []string{"jpg"}},
		OCR:               config.OCRConfig{ExcludeExtensions: []string{"pdf"}},
	}, Roots: []config.RootConfig{{
		ID: "inherit"}, {
		ID: "override", ContentExcludeExtensions: &empty, OCRExcludeExtensions: &empty,
	}}}
	c := New(cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := c.contentExcludedExtensions("inherit"); len(got) != 1 || got[0] != "jpg" {
		t.Fatalf("inherited content exclusions = %#v", got)
	}
	if got := c.ocrExcludedExtensions("inherit"); len(got) != 1 || got[0] != "pdf" {
		t.Fatalf("inherited OCR exclusions = %#v", got)
	}
	if got := c.contentExcludedExtensions("override"); got == nil || len(got) != 0 {
		t.Fatalf("content override = %#v", got)
	}
	if got := c.ocrExcludedExtensions("override"); got == nil || len(got) != 0 {
		t.Fatalf("OCR override = %#v", got)
	}
}

func TestRootWorkProfileCapsHighLatencyRoots(t *testing.T) {
	profile := capRootWorkProfile(rootWorkProfile{
		Latency:          150 * time.Millisecond,
		DirectoryWorkers: 16,
		MetadataWorkers:  32,
	})
	if profile.Class != "high_latency" || profile.DirectoryWorkers != 4 || profile.MetadataWorkers != 8 {
		t.Fatalf("unexpected high-latency profile: %#v", profile)
	}
}

func TestRootWorkProfileKeepsFastRootsAtConfiguredMaximum(t *testing.T) {
	profile := capRootWorkProfile(rootWorkProfile{
		Latency:          2 * time.Millisecond,
		DirectoryWorkers: 16,
		MetadataWorkers:  32,
	})
	if profile.Class != "fast" || profile.DirectoryWorkers != 16 || profile.MetadataWorkers != 32 {
		t.Fatalf("unexpected fast profile: %#v", profile)
	}
}

func TestCancelRootEnrichmentDoesNotInterruptOtherRoots(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	c := New(&config.Config{}, cat, slog.New(slog.NewTextHandler(io.Discard, nil)))

	aCtx, finishA, ok := c.beginBackgroundWork(ctx, "root-a", c.backgroundJobEpoch("root-a"))
	if !ok {
		t.Fatal("root-a work did not start")
	}
	bCtx, finishB, ok := c.beginBackgroundWork(ctx, "root-b", c.backgroundJobEpoch("root-b"))
	if !ok {
		t.Fatal("root-b work did not start")
	}
	c.CancelRootEnrichment("root-a")
	select {
	case <-aCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("root-a enrichment was not cancelled")
	}
	select {
	case <-bCtx.Done():
		t.Fatal("root-b enrichment was incorrectly cancelled")
	default:
	}
	finishA()
	if !c.WaitRootEnrichment(context.Background(), "root-a") {
		t.Fatal("root-a enrichment did not drain")
	}
	if _, _, valid := c.beginBackgroundWork(ctx, "root-a", 0); valid {
		t.Fatal("stale root-a job remained valid after cancellation")
	}
	finishB()
}

func TestRemovalHintDoesNotMarkDocumentsMissing(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	cat, err := catalog.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	doc := catalog.Document{ID: "existing", RootID: "root", Path: filepath.Join(rootPath, "existing.txt"), NormalizedPath: catalog.NormalizePath(filepath.Join(rootPath, "existing.txt")), Name: "existing.txt", Extension: "txt", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: catalog.Signature(1, modified)}
	if _, err := cat.UpsertDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	cr := New(&config.Config{Roots: []config.RootConfig{{ID: "root", Path: rootPath, Enabled: true}}}, cat, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run, err := cr.CrawlHint(ctx, "root", filepath.Join(rootPath, "existing.txt"), true)
	if err != nil || run.Status != "hint_ok" {
		t.Fatalf("removal hint = %#v, %v", run, err)
	}
	results, err := cat.Search(ctx, catalog.SearchRequest{Filters: catalog.SearchFilters{Roots: []string{"root"}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results.Results) != 1 || results.Results[0].Status != "active" {
		t.Fatalf("removal hint changed indexed state: %#v", results.Results)
	}
}

func TestShallowDirectoryHintDoesNotWalkDescendants(t *testing.T) {
	ctx := context.Background()
	rootPath := t.TempDir()
	nested := filepath.Join(rootPath, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "changed.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	cr := New(&config.Config{
		Roots:   []config.RootConfig{{ID: "root", Path: rootPath, Enabled: true}},
		Crawler: config.CrawlerConfig{MetadataWorkerCount: 1, MetadataQueueSize: 1},
	}, cat, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run, err := cr.CrawlHintShallow(ctx, "root", rootPath, false)
	if err != nil || run.Status != "hint_ok" {
		t.Fatalf("shallow hint = %#v, %v", run, err)
	}
	if run.FilesSeen != 1 {
		t.Fatalf("shallow hint saw %d entries, want only the directory itself", run.FilesSeen)
	}
}

func TestPausedCrawlerRejectsNewWorkBeforeClaimingRoot(t *testing.T) {
	rootPath := t.TempDir()
	c := New(&config.Config{Roots: []config.RootConfig{{ID: "root", Path: rootPath, Enabled: true}}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.Pause(time.Hour)

	hint, err := c.CrawlHintShallow(context.Background(), "root", rootPath, false)
	if err != nil {
		t.Fatal(err)
	}
	manual, err := c.CrawlRootManual(context.Background(), "root")
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range []catalog.CrawlRun{hint, manual} {
		if run.Status != "paused" {
			t.Fatalf("paused crawl status = %q, want paused", run.Status)
		}
	}
	if roots := c.ActiveRoots(); len(roots) != 0 {
		t.Fatalf("paused work claimed roots: %v", roots)
	}
}

func TestHardStopWaitsForExplicitResume(t *testing.T) {
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.CancelAllAndStop()
	if !c.IsPaused() || c.Stats().PauseReason != "stopped" {
		t.Fatalf("hard stop was not visible in crawler state: %#v", c.Stats())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := c.waitIfPaused(ctx, true); err != context.DeadlineExceeded {
		t.Fatalf("hard stop wait = %v, want deadline exceeded", err)
	}
	c.Resume()
	if c.IsPaused() {
		t.Fatal("resume did not clear hard stop")
	}
}

func TestNeedsStartupCrawlSkipsRecentlyCompletedRoot(t *testing.T) {
	completed := time.Now().UTC()
	root := config.RootConfig{ID: "complete", Enabled: true}
	if needsStartupCrawl(root, true, catalog.RootState{LastSuccessfulCrawlAt: &completed, LastCrawlStatus: "ok"}, completed, time.Hour) {
		t.Fatal("completed root should not crawl again at startup")
	}
	if !needsStartupCrawl(root, false, catalog.RootState{}, completed, time.Hour) {
		t.Fatal("new root should be crawled at startup")
	}
}

func TestNeedsStartupCrawlRunsOverdueRoot(t *testing.T) {
	now := time.Now().UTC()
	completed := now.Add(-7 * 24 * time.Hour)
	root := config.RootConfig{ID: "overdue", Enabled: true}
	if !needsStartupCrawl(root, true, catalog.RootState{LastSuccessfulCrawlAt: &completed, LastCrawlStatus: "ok"}, now, 6*time.Hour) {
		t.Fatal("root overdue during service downtime should crawl at startup")
	}
}

func TestNeedsStartupCrawlResumesInterruptedCompletedRoot(t *testing.T) {
	completed := time.Now().UTC()
	root := config.RootConfig{ID: "resume", Enabled: true}
	for _, status := range []string{"cancelled", "interrupted", "running", "hint_running"} {
		if !needsStartupCrawl(root, true, catalog.RootState{LastSuccessfulCrawlAt: &completed, LastCrawlStatus: status}, completed, time.Hour) {
			t.Fatalf("expected %s crawl to resume at startup", status)
		}
	}
}

func TestMatchesAnyIsWindowsPathAware(t *testing.T) {
	tests := []struct {
		path     string
		patterns []string
	}{
		{`D:\Finance\Report.xlsx`, []string{`d:\finance\*.xlsx`}},
		{`D:\Finance\Nested\Report.xlsx`, []string{`D:\Finance\**\*.xlsx`}},
		{`\\nas01\share\Projects\Budget.docx`, []string{`\\NAS01\share\**\*.docx`}},
		{`D:/node_modules/pkg/file.js`, []string{`**/node_modules/**`}},
	}

	for _, tt := range tests {
		if !matchesAny(tt.path, tt.patterns) {
			t.Fatalf("expected %q to match %v", tt.path, tt.patterns)
		}
	}
}

func TestMatchesAnyRejectsNonMatchingWindowsPattern(t *testing.T) {
	if matchesAny(`D:\Finance\Report.xlsx`, []string{`D:\Legal\*.xlsx`}) {
		t.Fatal("unexpected match")
	}
}

func TestMatchesAnyRecognizesRecoveryDirectoryItself(t *testing.T) {
	if !matchesAny(`X:\@Recently-Snapshot`, []string{"**/@Recently-Snapshot/**"}) {
		t.Fatal("expected recovery directory itself to match its exclusion pattern")
	}
	if !matchesAny(`X:\$RECYCLE.BIN`, []string{"**/$RECYCLE.BIN/**"}) {
		t.Fatal("expected recycle directory itself to match its exclusion pattern")
	}
}

func TestMatchesAnyNormalizesWindowsPathsForDoubleStarRules(t *testing.T) {
	if !matchesAny(`C:\Users\Test\AppData\Local\Temp\build\artifact.tmp`, []string{"**/AppData/Local/Temp/**"}) {
		t.Fatal("expected Windows path to match a slash-normalized double-star rule")
	}
}

func TestPathAllowedRejectsRecordsUnderExcludedFolder(t *testing.T) {
	root := config.RootConfig{ExcludeFolderPatterns: []string{"**/Private/**"}}
	if PathAllowed(root, `X:\Cases\Private`, "", true) {
		t.Fatal("excluded directory remained eligible")
	}
	if PathAllowed(root, `X:\Cases\Private\draft.docx`, "docx", false) {
		t.Fatal("file beneath excluded directory remained eligible")
	}
	if !PathAllowed(root, `X:\Cases\Public\brief.docx`, "docx", false) {
		t.Fatal("unrelated file was unexpectedly excluded")
	}
}

func TestPathAllowedWithGlobalRulesKeepsRootRulesClean(t *testing.T) {
	global := config.CrawlerConfig{
		ExcludeFilePatterns:   []string{"*.tmp"},
		ExcludeFolderPatterns: []string{"**/$RECYCLE.BIN/**"},
	}
	root := config.RootConfig{}
	if PathAllowedWithGlobalRules(root, global, `X:\$RECYCLE.BIN\gone.docx`, "docx", false) {
		t.Fatal("global folder exclusion did not apply to descendant")
	}
	if PathAllowedWithGlobalRules(root, global, `X:\Cases\draft.tmp`, "tmp", false) {
		t.Fatal("global file exclusion did not apply")
	}
	if !PathAllowedWithGlobalRules(root, global, `X:\Cases\brief.docx`, "docx", false) {
		t.Fatal("ordinary file was unexpectedly excluded")
	}
}

func TestPathAllowedRejectsRecordsUnderOperationalFolder(t *testing.T) {
	tests := []string{
		`C:\Program Files (x86)\Steam\steamapps\common\game\data.bin`,
		`E:\isos\minitool 12.3\Windows\WinSxS\Catalogs\package.cat`,
	}
	for _, path := range tests {
		if PathAllowed(config.RootConfig{}, path, filepath.Ext(path), false) {
			t.Fatalf("operational descendant remained eligible: %s", path)
		}
	}
	if !PathAllowed(config.RootConfig{}, `E:\client data\brief.docx`, "docx", false) {
		t.Fatal("unrelated file was unexpectedly excluded")
	}
}

func TestShouldSkipOfficeTemporaryLockFile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"~$Draft.docx", `\~$Draft.docx`} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.Name() == name && !shouldSkip(config.RootConfig{}, config.CrawlerConfig{}, `X:\Legal\`+name, entry, false) {
				t.Fatalf("expected Office temporary lock file %q to be skipped", name)
			}
		}
	}
}

func TestShouldSkipOperationalWindowsDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "probe"), 0755); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		`C:\System Volume Information`,
		`C:\Windows\System32`,
		`C:\Program Files\QIndexer`,
		`C:\ProgramData\QSurfer`,
		`C:\Users\Test\AppData\Local\Temp\qindexer-ocr-123`,
		`C:\Users\Test\AppData\Local\Microsoft\Windows\WebCache`,
		`C:\Windows\System32\sru`,
		`C:\Users\Test\AppData\Local\Temp\_MEI12345`,
		`C:\Users\Test\AppData\Local\Google\Chrome\User Data\Default\CacheStorage`,
	} {
		if !shouldSkip(config.RootConfig{}, config.CrawlerConfig{}, path, entries[0], false) {
			t.Fatalf("expected operational directory %q to be skipped", path)
		}
	}
}

func TestShouldSkipHonorsGlobalFolderExclusions(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	global := config.CrawlerConfig{ExcludeFolderPatterns: []string{"**/Confidential/**"}}
	if !shouldSkip(config.RootConfig{}, global, `X:\Cases\Confidential\memo.txt`, entries[0], false) {
		t.Fatal("global folder exclusion did not stop traversal")
	}
}

func TestConfiguredRootPathRequiresExactNonGlobMatch(t *testing.T) {
	if !isConfiguredRootPath(`X:\Legal`, `x:/legal`) {
		t.Fatal("expected Windows root paths to compare case-insensitively")
	}
	if isConfiguredRootPath(`X:\*`, `X:\Legal`) {
		t.Fatal("globbed root must not make each child a configured root")
	}
}

func TestDirectoryQueueInterleavesTopLevelBranches(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	queue := newDirQueue([]string{root})
	first := filepath.Join(root, "A", "one")
	second := filepath.Join(root, "B", "one")
	third := filepath.Join(root, "A", "two")
	queue.add(first)
	queue.add(second)
	queue.add(third)
	for index, expected := range []string{first, second, third} {
		got, ok := queue.next(context.Background())
		if !ok || got != expected {
			t.Fatalf("turn %d: got %q (ok=%v), want %q", index, got, ok, expected)
		}
		queue.done()
	}
}

func TestDiskBusyPercentUsesDeviceBusyTime(t *testing.T) {
	previous := map[string]disk.IOCountersStat{"disk0": {IoTime: 100}}
	current := map[string]disk.IOCountersStat{"disk0": {IoTime: 350}}
	if got := diskBusyPercent(previous, current, time.Second); got != 25 {
		t.Fatalf("expected 25%% busy, got %v", got)
	}
}

func TestExternalCPUPercentExcludesIndexerWork(t *testing.T) {
	if got := externalCPUPercent(50, 400, 8); got != 0 {
		t.Fatalf("expected no external CPU pressure, got %v", got)
	}
	if got := externalCPUPercent(75, 200, 8); got != 50 {
		t.Fatalf("expected 50%% external CPU pressure, got %v", got)
	}
}

func TestThroughputUsesRollingSample(t *testing.T) {
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	started := time.Now()
	c.sampleThroughput(started)
	atomic.AddInt64(&c.filesStatted, 12)
	atomic.AddInt64(&c.dirsRead, 4)
	atomic.AddInt64(&c.bytesStatted, 2048)
	c.sampleThroughput(started.Add(2 * time.Second))
	rate := c.currentThroughput()
	if rate.filesPerSecond != 6 || rate.dirsPerSecond != 2 || rate.bytesPerSecond != 1024 {
		t.Fatalf("rolling throughput = %#v", rate)
	}
	c.sampleThroughput(started.Add(3 * time.Second))
	rate = c.currentThroughput()
	if rate.filesPerSecond != 0 || rate.dirsPerSecond != 0 || rate.bytesPerSecond != 0 {
		t.Fatalf("idle rolling throughput = %#v", rate)
	}
}

func TestProgressIncludesPerRootRateAndQueuePressure(t *testing.T) {
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	finish := c.beginProgress("fast-root", "C:\\")
	defer finish()
	started := time.Now()
	c.sampleProgressThroughput(started)
	c.updateDirectoryProgress("fast-root", `C:\\work`, 2, 1)
	c.recordProgressFiles("fast-root", 12, 2048)
	c.recordProgressBatch("fast-root", 450, 1000)
	c.sampleProgressThroughput(started.Add(2 * time.Second))
	progress := c.ActiveProgress()
	if len(progress) != 1 {
		t.Fatalf("progress count = %d, want 1", len(progress))
	}
	item := progress[0]
	if item.FilesPerSecond != 6 || item.DirectoriesPerSecond != 0.5 || item.LogicalBytesPerSecond != 1024 {
		t.Fatalf("unexpected progress rate: %#v", item)
	}
	if item.MetadataQueueDepth != 450 || item.MetadataQueueCapacity != 1000 || item.DatabaseBatches != 1 {
		t.Fatalf("unexpected queue state: %#v", item)
	}
}

func TestAdaptivePressureRecoversAfterHealthySamples(t *testing.T) {
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.updateAdaptivePressure("cpu", 90, 0, 3, 3)
	if c.adaptiveStatus().Paused {
		t.Fatal("single pressure sample should not pause the crawler")
	}
	for range 2 {
		c.updateAdaptivePressure("cpu", 90, 0, 3, 3)
	}
	if !c.adaptiveStatus().Paused {
		t.Fatal("expected adaptive pause")
	}
	for range 3 {
		c.updateAdaptivePressure("", 10, 5, 3, 3)
	}
	if c.adaptiveStatus().Paused {
		t.Fatal("expected adaptive recovery")
	}
}

func TestExplicitAdaptiveBypassSkipsPause(t *testing.T) {
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.updateAdaptivePressure("disk", 0, 90, 1, 3)
	if err := c.waitIfPaused(context.Background(), false); err != nil {
		t.Fatalf("explicit adaptive bypass should ignore pause: %v", err)
	}
}

func TestNormalCrawlWaitsForAdaptivePause(t *testing.T) {
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.updateAdaptivePressure("disk", 0, 90, 1, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := c.waitIfPaused(ctx, true); err != context.DeadlineExceeded {
		t.Fatalf("normal crawl should wait until context ends, got %v", err)
	}
}

func TestRootWorkIsVisibleToStopAndWait(t *testing.T) {
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	workCtx, finish, started := c.beginRootWork(context.Background(), "share")
	if !started {
		t.Fatal("root work did not start")
	}
	if !c.IsRootRunning("share") {
		t.Fatal("root work was not registered")
	}
	if roots := c.CancelAll(); len(roots) != 1 || roots[0] != "share" {
		t.Fatalf("cancel roots = %#v, want share", roots)
	}
	select {
	case <-workCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("root work context was not cancelled")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if c.WaitAll(waitCtx) {
		t.Fatal("work reported stopped before it finalized")
	}
	finish()
	waitCtx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !c.WaitAll(waitCtx) {
		t.Fatal("work did not finalize after finish")
	}
}

func TestEnrichmentDoesNotWaitForWatcherHint(t *testing.T) {
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	atomic.AddInt64(&c.activeCrawls, 1)
	defer atomic.AddInt64(&c.activeCrawls, -1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.waitForEnrichmentWindow(ctx, "root-b"); err != nil {
		t.Fatalf("enrichment was blocked by a watcher hint: %v", err)
	}
}

func TestEnrichmentWaitsForFullCrawl(t *testing.T) {
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	atomic.AddInt64(&c.activeCrawls, 1)
	atomic.AddInt64(&c.activeFullCrawls, 1)
	defer atomic.AddInt64(&c.activeCrawls, -1)
	defer atomic.AddInt64(&c.activeFullCrawls, -1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.waitForEnrichmentWindow(ctx, "root-b"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("enrichment wait error = %v, want context deadline", err)
	}
}

func TestContentWorkerStatsExposeWaitingWorkerAndBufferedJob(t *testing.T) {
	c := New(&config.Config{Crawler: config.CrawlerConfig{ContentExtraction: config.ContentExtractionConfig{QueueSize: 1}}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	atomic.AddInt64(&c.activeFullCrawls, 1)
	defer atomic.AddInt64(&c.activeFullCrawls, -1)
	c.contentJobs <- backgroundJob{rootID: "root"}
	go c.contentWorker(ctx)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		stats := c.Stats()
		if stats.ContentWorkers == 1 && stats.ContentWaiting == 1 && stats.ContentBuffered == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("content worker stats never reflected blocked worker: %#v", c.Stats())
}

func TestRepeatedUnreachableLogsAreSuppressed(t *testing.T) {
	var buf bytes.Buffer
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(&buf, nil)))
	for range 3 {
		c.logSuppressed("root-stat:share:/missing", "root path stat failed", "no such file or directory", time.Hour, "root", "share")
	}
	text := buf.String()
	if got := strings.Count(text, "root path stat failed"); got != 1 {
		t.Fatalf("logged %d repeated failures, want 1: %s", got, text)
	}
	c.logSuppressed("root-stat:share:/missing", "root path stat failed", "permission denied", time.Hour, "root", "share")
	if got := strings.Count(buf.String(), "root path stat failed"); got != 2 {
		t.Fatalf("changed error was not logged: %s", buf.String())
	}
}

func TestUnreachableRootLeavesExistingDocumentStateUntouched(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	missingRoot := filepath.Join(t.TempDir(), "missing-share")
	cfg := &config.Config{Roots: []config.RootConfig{{ID: "share", Path: missingRoot, Enabled: true}}}
	doc := catalog.Document{
		ID:                 "existing",
		RootID:             "share",
		Path:               filepath.Join(missingRoot, "case.docx"),
		NormalizedPath:     catalog.NormalizePath(filepath.Join(missingRoot, "case.docx")),
		Name:               "case.docx",
		Extension:          "docx",
		Size:               1,
		ModifiedAt:         time.Now().UTC(),
		LastSeenGeneration: 1,
		Signature:          catalog.Signature(1, time.Now().UTC()),
		AccessStatus:       "metadata_readable",
	}
	if _, err := cat.UpsertDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	c := New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run, err := c.CrawlRoot(ctx, "share")
	if err == nil || run.Status != "unreachable" {
		t.Fatalf("crawl = %#v, err=%v; want unreachable error", run, err)
	}
	resp, err := cat.Search(ctx, catalog.SearchRequest{Filters: catalog.SearchFilters{Roots: []string{"share"}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Status != "active" || resp.Results[0].AccessStatus != "metadata_readable" {
		t.Fatalf("unreachable root changed existing document state: %#v", resp.Results)
	}
	states, err := cat.RootStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if states["share"].MissingCount != 0 {
		t.Fatalf("missing count = %d, want 0", states["share"].MissingCount)
	}
}

func TestManualCrawlRestoresMissingDocumentBehindStaleCheckpoint(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	rootDir := t.TempDir()
	path := filepath.Join(rootDir, "case.docx")
	if err := os.WriteFile(path, []byte("case"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := catalog.Document{
		ID: "existing", RootID: "share", Path: path, NormalizedPath: catalog.NormalizePath(path),
		Name: "case.docx", Extension: "docx", Size: info.Size(), ModifiedAt: info.ModTime(),
		LastSeenGeneration: 1, Signature: catalog.Signature(info.Size(), info.ModTime()), AccessStatus: "metadata_readable",
	}
	if _, err := cat.UpsertDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.MarkMissing(ctx, "share", 2, 3); err != nil {
		t.Fatal(err)
	}
	if err := cat.MarkDirectoryCheckpoint(ctx, "share", rootDir); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Roots: []config.RootConfig{{ID: "share", Path: rootDir, Enabled: true}}}
	c := New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run, err := c.CrawlRoot(ctx, "share")
	if err != nil || run.Status != "ok" {
		t.Fatalf("manual crawl = %#v, err=%v", run, err)
	}
	resp, err := cat.Search(ctx, catalog.SearchRequest{Filters: catalog.SearchFilters{Roots: []string{"share"}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var restored bool
	for _, result := range resp.Results {
		if result.Path == path && result.Status == "active" {
			restored = true
			break
		}
	}
	if !restored {
		t.Fatalf("manual crawl did not restore the missing document: %#v", resp.Results)
	}
	checkpointed, err := cat.DirectoryCheckpointed(ctx, "share", rootDir)
	if err != nil {
		t.Fatal(err)
	}
	if checkpointed {
		t.Fatal("successful full crawl left a stale resume checkpoint")
	}
}

func TestCrawlPrunesExcludedMissingRecordsInsteadOfRevivingThem(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	rootDir := t.TempDir()
	privateDir := filepath.Join(rootDir, "Private")
	if err := os.Mkdir(privateDir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(privateDir, "secret.docx")
	if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := catalog.Document{
		ID: "excluded", RootID: "share", Path: path, NormalizedPath: catalog.NormalizePath(path),
		Name: "secret.docx", Extension: "docx", Size: info.Size(), ModifiedAt: info.ModTime(),
		LastSeenGeneration: 1, Signature: catalog.Signature(info.Size(), info.ModTime()), AccessStatus: "metadata_readable",
	}
	if _, err := cat.UpsertDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.MarkMissing(ctx, "share", 2, 3); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{Roots: []config.RootConfig{{ID: "share", Path: rootDir, Enabled: true, ExcludeFolderPatterns: []string{"**/Private/**"}}}}
	c := New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run, err := c.CrawlRoot(ctx, "share")
	if err != nil || run.Status != "ok" {
		t.Fatalf("crawl = %#v, err=%v", run, err)
	}
	states, err := cat.RootStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if states["share"].MissingCount != 0 {
		t.Fatalf("excluded record remained missing: %#v", states["share"])
	}
	resp, err := cat.Search(ctx, catalog.SearchRequest{Filters: catalog.SearchFilters{Roots: []string{"share"}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range resp.Results {
		if result.Path == path {
			t.Fatalf("excluded record was revived: %#v", result)
		}
	}
}

func TestHintStatFailureLeavesExistingDocumentStateUntouched(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	rootDir := t.TempDir()
	path := filepath.Join(rootDir, "case.docx")
	modified := time.Now().UTC()
	doc := catalog.Document{
		ID:                 "existing",
		RootID:             "share",
		Path:               path,
		NormalizedPath:     catalog.NormalizePath(path),
		Name:               "case.docx",
		Extension:          "docx",
		Size:               1,
		ModifiedAt:         modified,
		LastSeenGeneration: 1,
		Signature:          catalog.Signature(1, modified),
		AccessStatus:       "metadata_readable",
	}
	if _, err := cat.UpsertDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Roots: []config.RootConfig{{ID: "share", Path: rootDir, Enabled: true}}}
	c := New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run, err := c.CrawlHint(ctx, "share", path, false)
	if err != nil || run.Status != "hint_unreachable" {
		t.Fatalf("hint = %#v, err=%v; want hint_unreachable without catalog error", run, err)
	}
	resp, err := cat.Search(ctx, catalog.SearchRequest{Filters: catalog.SearchFilters{Roots: []string{"share"}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Status != "active" || resp.Results[0].AccessStatus != "metadata_readable" {
		t.Fatalf("hint stat failure changed existing document state: %#v", resp.Results)
	}
	states, err := cat.RootStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if states["share"].MissingCount != 0 {
		t.Fatalf("missing count = %d, want 0", states["share"].MissingCount)
	}
}

func TestHintIgnoresOperationalDirectory(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	rootDir := t.TempDir()
	path := filepath.Join(rootDir, "System Volume Information")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(&config.Config{Roots: []config.RootConfig{{ID: "root", Path: `C:\`, Enabled: true}}}, cat, slog.New(slog.NewTextHandler(io.Discard, nil)))
	run, err := c.CrawlHint(ctx, "root", path, false)
	if err != nil || run.Status != "hint_ignored" {
		t.Fatalf("hint = %#v, err=%v; want hint_ignored", run, err)
	}
}

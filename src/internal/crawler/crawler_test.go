package crawler

import (
	"bytes"
	"context"
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

func TestNeedsInitialCrawlSkipsCompletedRoot(t *testing.T) {
	completed := time.Now().UTC()
	root := config.RootConfig{ID: "complete", Enabled: true}
	if needsInitialCrawl(root, true, catalog.RootState{LastSuccessfulCrawlAt: &completed}) {
		t.Fatal("completed root should not crawl again at startup")
	}
	if !needsInitialCrawl(root, false, catalog.RootState{}) {
		t.Fatal("new root should be crawled at startup")
	}
}

func TestNeedsInitialCrawlResumesInterruptedCompletedRoot(t *testing.T) {
	completed := time.Now().UTC()
	root := config.RootConfig{ID: "resume", Enabled: true}
	for _, status := range []string{"cancelled", "interrupted", "running", "hint_running"} {
		if !needsInitialCrawl(root, true, catalog.RootState{LastSuccessfulCrawlAt: &completed, LastCrawlStatus: status}) {
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

func TestAdaptivePressureRecoversAfterHealthySamples(t *testing.T) {
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.updateAdaptivePressure("cpu", 90, 0, 3)
	if !c.adaptiveStatus().Paused {
		t.Fatal("expected adaptive pause")
	}
	for range 3 {
		c.updateAdaptivePressure("", 10, 5, 3)
	}
	if c.adaptiveStatus().Paused {
		t.Fatal("expected adaptive recovery")
	}
}

func TestInitialCrawlIgnoresAdaptivePause(t *testing.T) {
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.updateAdaptivePressure("disk", 0, 90, 3)
	if err := c.waitIfPaused(context.Background(), false); err != nil {
		t.Fatalf("initial crawl should ignore adaptive pause: %v", err)
	}
}

func TestNormalCrawlWaitsForAdaptivePause(t *testing.T) {
	c := New(&config.Config{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.updateAdaptivePressure("disk", 0, 90, 3)
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

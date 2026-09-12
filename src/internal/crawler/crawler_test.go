package crawler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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

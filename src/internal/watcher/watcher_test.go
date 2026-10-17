package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"

	"qindexer/internal/config"
)

func TestPinnedWatchLimitReservesCapacityForHotFolders(t *testing.T) {
	if got := pinnedWatchLimit(25_000, 1); got != 15_000 {
		t.Fatalf("pinned watch limit = %d, want 15000", got)
	}
	if got := pinnedWatchLimit(3, 5); got != 3 {
		t.Fatalf("pinned watch limit = %d, want 3", got)
	}
	if got := pinnedWatchLimit(0, 1); got != 0 {
		t.Fatalf("zero watch limit = %d, want 0", got)
	}
}

func TestTopLevelCandidateLimitIsBoundedPerRoot(t *testing.T) {
	set := &watchSet{pinned: map[string]bool{}, pinnedLimit: 60}
	if got := topLevelCandidateLimit(set, 3); got != 20 {
		t.Fatalf("candidate limit = %d, want 20", got)
	}
	set.pinned["root"] = true
	if got := topLevelCandidateLimit(set, 3); got != 19 {
		t.Fatalf("candidate limit after root = %d, want 19", got)
	}
}

func TestReadDirWithTimeoutCapsEntries(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := readDirWithTimeout(context.Background(), dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entry count = %d, want 2", len(entries))
	}
}

func TestMergeDirtyPathsCoalescesLatestEvent(t *testing.T) {
	pending := map[string]map[string]dirtyPath{}
	mergeDirtyPaths(pending, "root", map[string]dirtyPath{"one": {path: "one"}})
	mergeDirtyPaths(pending, "root", map[string]dirtyPath{
		"one": {path: "one", removed: true},
		"two": {path: "two"},
	})
	if len(pending["root"]) != 2 || !pending["root"]["one"].removed {
		t.Fatalf("coalesced dirty paths = %#v", pending["root"])
	}
}

func TestFilterRecentHintsSuppressesRepeatedPathUntilCooldownExpires(t *testing.T) {
	w := &Watcher{hintsAt: map[string]time.Time{}}
	paths := map[string]dirtyPath{"C:/work/example.txt": {path: "C:/work/example.txt"}}
	first := w.filterRecentHints("root", paths, 15*time.Minute)
	if len(first) != 1 {
		t.Fatalf("first hint count = %d, want 1", len(first))
	}
	second := w.filterRecentHints("root", paths, 15*time.Minute)
	if len(second) != 0 {
		t.Fatalf("repeat hint count = %d, want 0", len(second))
	}
	w.releaseHint("root", "C:/work/example.txt")
	third := w.filterRecentHints("root", paths, 15*time.Minute)
	if len(third) != 1 {
		t.Fatalf("released hint count = %d, want 1", len(third))
	}
}

func TestHintBatchWindowDefersRepeatedRootDispatch(t *testing.T) {
	w := &Watcher{hintsAt: map[string]time.Time{}, rootHintsAt: map[string]time.Time{}}
	if !w.waitForHintBatchWindow(context.Background(), "root", time.Minute) {
		t.Fatal("initial hint batch was not accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if w.waitForHintBatchWindow(ctx, "root", time.Minute) {
		t.Fatal("repeated root hint was not deferred")
	}
}

func TestShouldQueueEventAvoidsNoiseButRefreshesEligibleEnrichment(t *testing.T) {
	rootPath := t.TempDir()
	cfg := &config.Config{Roots: []config.RootConfig{{ID: "root", Path: rootPath, Enabled: true}}, Crawler: config.CrawlerConfig{
		ExcludeFilePatterns: []string{"~$*"},
		ContentExtraction:   config.ContentExtractionConfig{Enabled: true},
		OCR:                 config.OCRConfig{Enabled: true},
	}}
	w := &Watcher{}
	if w.shouldQueueEvent(cfg, "root", fsnotify.Event{Name: filepath.Join(rootPath, "~$draft.docx"), Op: fsnotify.Create}) {
		t.Fatal("temporary Office file was queued")
	}
	if !w.shouldQueueEvent(cfg, "root", fsnotify.Event{Name: filepath.Join(rootPath, "notes.txt"), Op: fsnotify.Write}) {
		t.Fatal("content-eligible write was not queued")
	}
	if !w.shouldQueueEvent(cfg, "root", fsnotify.Event{Name: filepath.Join(rootPath, "scan.pdf"), Op: fsnotify.Write}) {
		t.Fatal("OCR-eligible write was not queued")
	}
	if actionableEvent(fsnotify.Event{Op: fsnotify.Chmod}) {
		t.Fatal("attribute-only event was queued")
	}
}

func TestExcludedPathsNeverBecomeHintsOrDirectoryWatches(t *testing.T) {
	rootPath := t.TempDir()
	privateDir := filepath.Join(rootPath, "Private")
	if err := os.Mkdir(privateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	excludedFile := filepath.Join(rootPath, ".temp_writeABCD$")
	if err := os.WriteFile(excludedFile, []byte("temporary"), 0o600); err != nil {
		t.Fatal(err)
	}
	ordinaryFile := filepath.Join(rootPath, "brief.txt")
	if err := os.WriteFile(ordinaryFile, []byte("matter"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Roots: []config.RootConfig{{ID: "root", Path: rootPath, Enabled: true, ExcludeFolderPatterns: []string{"**/Private/**"}}}, Crawler: config.CrawlerConfig{
		ExcludeFilePatterns: []string{".temp_write*"},
	}}
	w := &Watcher{}
	root, ok := rootConfig(cfg, "root")
	if !ok {
		t.Fatal("enabled root was not found")
	}
	if w.directoryWatchAllowed(cfg, root, privateDir) {
		t.Fatal("excluded folder was eligible for a directory watch")
	}
	if !w.directoryWatchAllowed(cfg, root, rootPath) {
		t.Fatal("file exclusion incorrectly made the parent directory ineligible")
	}
	paths := w.filterEligibleHints(cfg, "root", map[string]dirtyPath{
		excludedFile: {path: excludedFile},
		privateDir:   {path: privateDir, recursive: true},
		ordinaryFile: {path: ordinaryFile},
	})
	if len(paths) != 1 || paths[ordinaryFile].path != ordinaryFile {
		t.Fatalf("eligible hints = %#v, want only ordinary file", paths)
	}
	if w.shouldQueueEvent(cfg, "root", fsnotify.Event{Name: privateDir, Op: fsnotify.Create}) {
		t.Fatal("excluded folder create was queued")
	}
}

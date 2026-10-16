package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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

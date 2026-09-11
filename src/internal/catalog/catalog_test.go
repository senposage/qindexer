package catalog

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestSearchPaginationFoldersAndSegmentAwareScopes(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	modified := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	add := func(path string, folder bool) {
		t.Helper()
		doc := Document{
			ID:                 path,
			RootID:             "drive-c",
			Path:               path,
			NormalizedPath:     NormalizePath(path),
			Name:               filepath.Base(path),
			Extension:          filepath.Ext(path),
			IsFolder:           folder,
			Size:               10,
			ModifiedAt:         modified,
			LastSeenGeneration: 1,
			Signature:          Signature(10, modified),
		}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	add(`C:\Legal`, true)
	add(`C:\Legal\brief.docx`, false)
	add(`C:\Legal\notes.txt`, false)
	add(`C:\Legalities\different.docx`, false)

	resp, err := cat.Search(ctx, SearchRequest{
		SearchID: "request-1",
		Filters:  SearchFilters{PathPrefixes: []string{`C:\Legal`}},
		Sort:     "name",
		Limit:    2,
	}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if resp.SearchID != "request-1" || !resp.HasMore || resp.NextOffset == nil || *resp.NextOffset != 2 {
		t.Fatalf("unexpected page metadata: %#v", resp)
	}
	for _, result := range resp.Results {
		if result.Path == `C:\Legalities\different.docx` {
			t.Fatal("segment-aware scope included Legalities")
		}
		if result.ResultID == "" || result.ETag == "" || result.Kind == "" {
			t.Fatalf("missing stable result fields: %#v", result)
		}
	}

	folders, err := cat.Search(ctx, SearchRequest{Filters: SearchFilters{Kind: "folder"}, Limit: 10}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders.Results) != 1 || !folders.Results[0].IsFolder || folders.Results[0].Kind != "folder" {
		t.Fatalf("unexpected folders: %#v", folders.Results)
	}

	empty, err := cat.Search(ctx, SearchRequest{Query: "no-such-match", Limit: 10}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if empty.Results == nil || len(empty.Results) != 0 {
		t.Fatalf("expected a non-nil empty result list, got %#v", empty.Results)
	}
}

func TestDescendantPathLikePreservesDriveRootSeparator(t *testing.T) {
	if got := descendantPathLike(`d:\`); got != `d:\%` {
		t.Fatalf("unexpected drive-root pattern %q", got)
	}
	if got := descendantPathLike(`d:\legal`); got != `d:\legal\%` {
		t.Fatalf("unexpected nested pattern %q", got)
	}
}

func TestContentMatchMetadataUsesBoundedExcerpt(t *testing.T) {
	doc := Document{Name: "report.docx", Path: `C:\Finance\report.docx`, ContentText: "The quarterly revenue plan is ready for the finance review."}
	addMatchMetadata(&doc, "revenue")
	if len(doc.MatchedFields) != 1 || doc.MatchedFields[0] != "content" {
		t.Fatalf("unexpected match fields: %#v", doc.MatchedFields)
	}
	if got := doc.Highlights["content"][0]; got == "" || len(got) > 192 {
		t.Fatalf("unexpected content excerpt: %q", got)
	}
}

func TestSearchMatchesFilenamePrefix(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	doc := Document{ID: "zankyo", RootID: "drive-d", Path: `D:\zankyo.docx`, NormalizedPath: NormalizePath(`D:\zankyo.docx`), Name: "zankyo.docx", Extension: "docx", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
	if _, err := cat.UpsertDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	resp, err := cat.Search(ctx, SearchRequest{Query: "zan", Limit: 10}, 10)
	if err != nil || len(resp.Results) != 1 || resp.Results[0].Path != doc.Path {
		t.Fatalf("prefix query did not match: %#v err=%v", resp.Results, err)
	}
}

func TestClearRootRemovesOnlyThatRoot(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	for _, rootID := range []string{"finance", "engineering"} {
		doc := Document{ID: rootID, RootID: rootID, Path: `D:\` + rootID + `\report.txt`, NormalizedPath: NormalizePath(`D:\` + rootID + `\report.txt`), Name: "report.txt", Extension: "txt", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := cat.ClearRoot(ctx, "finance")
	if err != nil || removed != 1 {
		t.Fatalf("clear root = %d, %v", removed, err)
	}
	remaining, err := cat.Search(ctx, SearchRequest{Limit: 10}, 20)
	if err != nil || len(remaining.Results) != 1 || remaining.Results[0].RootID != "engineering" {
		t.Fatalf("unexpected remaining documents: %#v, %v", remaining.Results, err)
	}
}

func TestSpecificIncludeOverridesParentExclusion(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	for _, path := range []string{`D:\Legal\Matter\brief.docx`, `D:\Legal\Archive\old.docx`} {
		_, err := cat.UpsertDocument(ctx, Document{ID: path, RootID: "drive-d", Path: path, NormalizedPath: NormalizePath(path), Name: filepath.Base(path), Extension: "docx", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)})
		if err != nil {
			t.Fatal(err)
		}
	}
	resp, err := cat.Search(ctx, SearchRequest{Filters: SearchFilters{IncludePaths: []string{`D:\Legal\Matter`}, ExcludePaths: []string{`D:\Legal`}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Path != `D:\Legal\Matter\brief.docx` {
		t.Fatalf("child include did not override parent exclusion: %#v", resp.Results)
	}
}

func TestNewPathReportsWatcherDetectedMove(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	oldPath, newPath := `D:\Old\report.docx`, `D:\New\report.docx`
	base := Document{RootID: "drive-d", Extension: "docx", Size: 2, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(2, modified)}
	base.ID, base.Path, base.NormalizedPath, base.Name = "old", oldPath, NormalizePath(oldPath), filepath.Base(oldPath)
	if _, err := cat.UpsertDocument(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.MarkPathMissing(ctx, "drive-d", oldPath); err != nil {
		t.Fatal(err)
	}
	base.ID, base.Path, base.NormalizedPath, base.Name = "new", newPath, NormalizePath(newPath), filepath.Base(newPath)
	if _, err := cat.UpsertDocument(ctx, base); err != nil {
		t.Fatal(err)
	}
	resp, err := cat.Search(ctx, SearchRequest{Filters: SearchFilters{Roots: []string{"drive-d"}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].MovedFromPath != oldPath {
		t.Fatalf("move relationship missing: %#v", resp.Results)
	}
}

func TestReconcileMovesUsesOnlyUnambiguousHashes(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	add := func(id, path, status, hash string, generation int64) {
		t.Helper()
		doc := Document{ID: id, RootID: "drive-d", Path: path, NormalizedPath: NormalizePath(path), Name: filepath.Base(path), Extension: "txt", Size: 1, ModifiedAt: modified, LastSeenGeneration: generation, Signature: Signature(1, modified) + ":" + id}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if _, err := cat.db.ExecContext(ctx, `UPDATE documents SET status = ?, content_hash = ? WHERE id = ?`, status, hash, id); err != nil {
			t.Fatal(err)
		}
	}
	add("old", `D:\Old\a.txt`, "missing", "sha256:abc", 1)
	add("new", `D:\New\a.txt`, "active", "sha256:abc", 2)
	moved, err := cat.ReconcileMoves(ctx, "drive-d", 2)
	if err != nil || moved != 1 {
		t.Fatalf("reconcile result: moved=%d err=%v", moved, err)
	}
	resp, err := cat.Search(ctx, SearchRequest{Filters: SearchFilters{Roots: []string{"drive-d"}}, Limit: 10}, 10)
	if err != nil || len(resp.Results) != 1 || resp.Results[0].MovedFromPath != `D:\Old\a.txt` {
		t.Fatalf("move metadata missing: %#v err=%v", resp.Results, err)
	}
}

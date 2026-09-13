package catalog

import (
	"context"
	"path/filepath"
	"strings"
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

func TestCheckDatabaseUsesReadOnlySQLiteURI(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cat, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckDatabase(ctx, filepath.Join(dir, "qsurfer-search.db")); err != nil {
		t.Fatalf("read-only database check failed: %v", err)
	}
}

func TestUpsertDocumentsRestoresMissingRecordsInOneBatch(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	makeDoc := func(id, path string, generation int64) Document {
		return Document{ID: id, RootID: "test", Path: path, NormalizedPath: NormalizePath(path), Name: filepath.Base(path), Extension: "txt", Size: 1, ModifiedAt: modified, LastSeenGeneration: generation, Signature: Signature(1, modified)}
	}
	if _, err := cat.UpsertDocuments(ctx, []Document{makeDoc("one", `D:\fixtures\one.txt`, 1), makeDoc("two", `D:\fixtures\two.txt`, 1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.MarkMissing(ctx, "test", 2, 3); err != nil {
		t.Fatal(err)
	}
	paths, err := cat.MissingDocumentPaths(ctx, "test", 10)
	if err != nil || len(paths) != 2 {
		t.Fatalf("missing paths = %#v, %v", paths, err)
	}
	results, err := cat.UpsertDocuments(ctx, []Document{makeDoc("replacement-one", `D:\fixtures\one.txt`, 3), makeDoc("replacement-two", `D:\fixtures\two.txt`, 3)})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || !results[0].Unchanged || !results[1].Unchanged {
		t.Fatalf("unexpected batch restore results: %#v", results)
	}
	paths, err = cat.MissingDocumentPaths(ctx, "test", 10)
	if err != nil || len(paths) != 0 {
		t.Fatalf("restored records still missing: %#v, %v", paths, err)
	}
}

func TestContentMatchMetadataUsesBoundedExcerpt(t *testing.T) {
	doc := Document{Name: "report.docx", Path: `C:\Finance\report.docx`, ContentText: "The quarterly revenue plan is ready for the finance review."}
	addMatchMetadata(&doc, "revenue", nil)
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

func TestSearchFoldersMustMatchTheirOwnName(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	modified := time.Now().UTC()
	add := func(id, path string, folder bool) {
		t.Helper()
		doc := Document{
			ID:                 id,
			RootID:             "shared",
			Path:               path,
			NormalizedPath:     NormalizePath(path),
			Name:               filepath.Base(path),
			Extension:          strings.TrimPrefix(filepath.Ext(path), "."),
			IsFolder:           folder,
			Size:               1,
			ModifiedAt:         modified,
			LastSeenGeneration: 1,
			Signature:          Signature(1, modified),
		}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	add("billing-folder", `X:\AA _ OFFICE ADMINISTRATION\BILLING`, true)
	add("office-file", `X:\AA _ OFFICE ADMINISTRATION\report.txt`, false)

	resp, err := cat.Search(ctx, SearchRequest{
		Query:   "office",
		Filters: SearchFilters{MatchFields: []string{"name", "path"}},
		Limit:   10,
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].ID != "office-file" {
		t.Fatalf("ancestor path match returned a folder: %#v", resp.Results)
	}

	folders, err := cat.Search(ctx, SearchRequest{Query: "billing", Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(folders.Results) != 1 || folders.Results[0].ID != "billing-folder" {
		t.Fatalf("folder name query did not return BILLING: %#v", folders.Results)
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

func TestRewriteRootPathMergesDuplicateActiveRows(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	modified := time.Now().UTC()
	oldPath := `X:\Legal\brief.docx`
	newRoot := `\\fileserver.example.test\mounts\team-share`
	newPath := newRoot + `\Legal\brief.docx`
	for _, item := range []struct {
		id   string
		path string
	}{
		{"old", oldPath},
		{"new", newPath},
	} {
		doc := Document{ID: item.id, RootID: "shared", Path: item.path, NormalizedPath: NormalizePath(item.path), Name: filepath.Base(item.path), Extension: "docx", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	rewritten, merged, err := cat.RewriteRootPath(ctx, "shared", `X:\`, newRoot)
	if err != nil {
		t.Fatal(err)
	}
	if rewritten != 0 || merged != 1 {
		t.Fatalf("expected one duplicate merge, got rewritten=%d merged=%d", rewritten, merged)
	}
	resp, err := cat.Search(ctx, SearchRequest{Query: "brief", Filters: SearchFilters{Roots: []string{"shared"}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Path != newPath {
		t.Fatalf("duplicate root paths remained: %#v", resp.Results)
	}
}

func TestRewriteRootPathUpdatesPathAndFTS(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	modified := time.Now().UTC()
	oldPath := `X:\Cases\Budget.docx`
	newRoot := `/mnt/shared-real`
	doc := Document{ID: "old", RootID: "shared", Path: oldPath, NormalizedPath: NormalizePath(oldPath), Name: filepath.Base(oldPath), Extension: "docx", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
	if _, err := cat.UpsertDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	rewritten, merged, err := cat.RewriteRootPath(ctx, "shared", `X:\`, newRoot)
	if err != nil {
		t.Fatal(err)
	}
	if rewritten != 1 || merged != 0 {
		t.Fatalf("expected one rewrite, got rewritten=%d merged=%d", rewritten, merged)
	}
	resp, err := cat.Search(ctx, SearchRequest{Query: "shared-real", Filters: SearchFilters{Roots: []string{"shared"}, MatchFields: []string{"path"}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Path != `/mnt/shared-real/Cases/Budget.docx` {
		t.Fatalf("rewritten path not searchable: %#v", resp.Results)
	}
}

func TestRepairEmbeddedRootPathMergesUNCWrappedCanonicalPath(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	modified := time.Now().UTC()
	root := `/srv/qindexer/mounts/team-share`
	wrapped := `\\fileserver.example.test\archives\srv\qindexer\mounts\team-share\Legal\Example\affidavit.docx`
	canonical := root + `/Legal/Example/affidavit.docx`
	for _, item := range []struct {
		id   string
		path string
	}{
		{"wrapped", wrapped},
		{"canonical", canonical},
	} {
		doc := Document{ID: item.id, RootID: "shared", Path: item.path, NormalizedPath: NormalizePath(item.path), Name: filepath.Base(item.path), Extension: "docx", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	rewritten, merged, err := cat.RepairEmbeddedRootPath(ctx, "shared", root)
	if err != nil {
		t.Fatal(err)
	}
	if rewritten != 0 || merged != 1 {
		t.Fatalf("expected one embedded duplicate merge, got rewritten=%d merged=%d", rewritten, merged)
	}
	resp, err := cat.Search(ctx, SearchRequest{Query: "affidavit", Filters: SearchFilters{Roots: []string{"shared"}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Path != canonical {
		t.Fatalf("embedded path repair left duplicate or wrong path: %#v", resp.Results)
	}
}

func TestMarkOpenCrawlsInterrupted(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if _, err := cat.NextGeneration(ctx, "shared"); err != nil {
		t.Fatal(err)
	}
	if err := cat.StartCrawl(ctx, CrawlRun{ID: "run-1", RootID: "shared", Generation: 1, StartedAt: time.Now().UTC(), Status: "running"}); err != nil {
		t.Fatal(err)
	}
	changed, err := cat.MarkOpenCrawlsInterrupted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 1 {
		t.Fatalf("changed = %d, want 1", changed)
	}
	runs, err := cat.RecentCrawls(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Status != "interrupted" || runs[0].FinishedAt == nil {
		t.Fatalf("open crawl was not marked interrupted: %#v", runs)
	}
	states, err := cat.RootStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if states["shared"].LastCrawlStatus != "interrupted" || states["shared"].LastError == "" {
		t.Fatalf("root state was not marked interrupted: %#v", states["shared"])
	}
}

func TestPruneRecoveryPathsRemovesOnlyRecoveryDirectories(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	modified := time.Now().UTC()
	add := func(id, rootID, path string) {
		t.Helper()
		doc := Document{ID: id, RootID: rootID, Path: path, NormalizedPath: NormalizePath(path), Name: filepath.Base(path), Extension: strings.TrimPrefix(filepath.Ext(path), "."), Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	add("snapshot", "shared", `X:\@Recently-Snapshot\GMT-05_2026-09-04_0000\budget.txt`)
	add("recycle", "shared", `X:\$RECYCLE.BIN\S-1-5-21\deleted.txt`)
	add("keep", "shared", `X:\AA _ OFFICE ADMINISTRATION\billing.txt`)
	add("other-root", "other", `X:\@Recently-Snapshot\other.txt`)

	removed, err := cat.PruneRecoveryPaths(ctx, "shared")
	if err != nil || removed != 2 {
		all, searchErr := cat.Search(ctx, SearchRequest{Limit: 10}, 10)
		t.Fatalf("prune recovery paths = %d, %v; remaining=%#v, searchErr=%v", removed, err, all.Results, searchErr)
	}
	resp, err := cat.Search(ctx, SearchRequest{Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 2 {
		t.Fatalf("unexpected remaining paths: %#v", resp.Results)
	}
	for _, result := range resp.Results {
		if result.ID == "snapshot" || result.ID == "recycle" {
			t.Fatalf("recovery path remained indexed: %#v", result)
		}
	}
}

func TestSearchOrderKeepsDescendingRelevanceBestFirst(t *testing.T) {
	order, err := searchOrder("relevance", "desc", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(order, "bm25(documents_fts) ASC") {
		t.Fatalf("expected best-first relevance to use ascending bm25, got %q", order)
	}
	order, err = searchOrder("relevance", "asc", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(order, "bm25(documents_fts) DESC") {
		t.Fatalf("expected ascending relevance to use descending bm25, got %q", order)
	}
}

func TestSearchMatchFieldsSeparatesMetadataAndContent(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	doc := Document{ID: "content-only", RootID: "test", Path: `D:\fixtures\content\plain-notes.txt`, NormalizedPath: NormalizePath(`D:\fixtures\content\plain-notes.txt`), Name: "plain-notes.txt", Extension: "txt", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
	if _, err := cat.UpsertDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateExtractedContent(ctx, doc.ID, doc.Signature, "extracted", "QINDEXER ORCHID 731"); err != nil {
		t.Fatal(err)
	}
	metadata, err := cat.Search(ctx, SearchRequest{Query: "orchid", Filters: SearchFilters{MatchFields: []string{"name", "path", "extension"}}, Limit: 10}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.Results) != 0 {
		t.Fatalf("metadata-only search returned content match: %#v", metadata.Results)
	}
	content, err := cat.Search(ctx, SearchRequest{Query: "orchid", Filters: SearchFilters{MatchFields: []string{"content"}}, Limit: 10}, 20)
	if err != nil || len(content.Results) != 1 || len(content.Results[0].MatchedFields) != 1 || content.Results[0].MatchedFields[0] != "content" {
		t.Fatalf("content-only search = %#v, %v", content.Results, err)
	}
	matchDoc := Document{Path: `D:\fixtures\orchid\notes.txt`, Name: "notes.txt", ContentText: "ORCHID"}
	addMatchMetadata(&matchDoc, "orchid", []string{"content"})
	if len(matchDoc.MatchedFields) != 1 || matchDoc.MatchedFields[0] != "content" {
		t.Fatalf("content-only match metadata leaked other fields: %#v", matchDoc.MatchedFields)
	}
	if _, err := cat.Search(ctx, SearchRequest{Query: "orchid", Filters: SearchFilters{MatchFields: []string{"unknown"}}, Limit: 10}, 20); err == nil {
		t.Fatal("expected invalid match field error")
	}
}

func TestClaimPendingWorkRespectsRootSelection(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	for _, rootID := range []string{"enabled", "disabled"} {
		doc := Document{ID: rootID, RootID: rootID, Path: `D:\` + rootID + `\file.txt`, NormalizedPath: NormalizePath(`D:\` + rootID + `\file.txt`), Name: "file.txt", Extension: "txt", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	items, err := cat.ClaimPendingContent(ctx, []string{"enabled"}, 10, 1024)
	if err != nil || len(items) != 1 || items[0].RootID != "enabled" {
		t.Fatalf("claimed items = %#v, %v", items, err)
	}
	items, err = cat.ClaimPendingContent(ctx, []string{"disabled"}, 10, 1024)
	if err != nil || len(items) != 1 || items[0].RootID != "disabled" {
		t.Fatalf("claimed items = %#v, %v", items, err)
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

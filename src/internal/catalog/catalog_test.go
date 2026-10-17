package catalog

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestPruneDocumentsRemovesExcludedRecordsRegardlessOfStatus(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	modified := time.Now().UTC()
	add := func(id, path, status string) {
		t.Helper()
		doc := Document{
			ID: id, RootID: "shared", Path: path, NormalizedPath: NormalizePath(path),
			Name: filepath.Base(path), Extension: "docx", Size: 1, ModifiedAt: modified,
			LastSeenGeneration: 1, Signature: Signature(1, modified),
		}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if _, err := cat.db.ExecContext(ctx, `UPDATE documents SET status = ? WHERE id = ?`, status, id); err != nil {
			t.Fatal(err)
		}
	}
	add("active-private", `X:\Cases\Private\draft.docx`, "active")
	add("missing-private", `X:\Cases\Private\old.docx`, "missing")
	add("active-public", `X:\Cases\Public\brief.docx`, "active")

	removed, err := cat.PruneDocuments(ctx, "shared", func(path, _ string, _ bool) bool {
		return !strings.Contains(path, `\Private\`)
	})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("removed %d records, want 2", removed)
	}
	var count int
	if err := cat.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents WHERE root_id = 'shared'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("remaining documents %d, want 1", count)
	}
	var ftsCount int
	if err := cat.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents_fts`).Scan(&ftsCount); err != nil {
		t.Fatal(err)
	}
	if ftsCount != 1 {
		t.Fatalf("remaining FTS records %d, want 1", ftsCount)
	}
}

func TestResetQueuedBackgroundForRootIsScoped(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	for _, rootID := range []string{"root-a", "root-b"} {
		doc := Document{ID: rootID, RootID: rootID, Path: rootID + ".pdf", NormalizedPath: NormalizePath(rootID + ".pdf"), Name: rootID + ".pdf", Extension: "pdf", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if _, err := cat.db.ExecContext(ctx, `UPDATE documents SET content_status = 'queued', ocr_status = 'queued', hash_status = 'queued' WHERE id = ?`, rootID); err != nil {
			t.Fatal(err)
		}
	}
	if err := cat.ResetQueuedBackgroundForRoot(ctx, "root-a"); err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct{ rootID, want string }{{"root-a", "not_indexed"}, {"root-b", "queued"}} {
		var content string
		if err := cat.db.QueryRowContext(ctx, `SELECT content_status FROM documents WHERE root_id = ?`, check.rootID).Scan(&content); err != nil {
			t.Fatal(err)
		}
		if content != check.want {
			t.Fatalf("%s content status = %q, want %q", check.rootID, content, check.want)
		}
	}
}

func TestMarkPathInaccessibleKeepsDocumentActive(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	path := `X:\Private\locked.docx`
	if _, err := cat.UpsertDocument(ctx, Document{ID: "locked", RootID: "root", Path: path, NormalizedPath: NormalizePath(path), Name: "locked.docx", Extension: "docx", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}); err != nil {
		t.Fatal(err)
	}
	if _, err := cat.MarkPathInaccessible(ctx, "root", path, 2); err != nil {
		t.Fatal(err)
	}
	var status, accessStatus string
	var generation, missingCount int64
	if err := cat.db.QueryRowContext(ctx, `SELECT status, access_status, last_seen_generation, missing_count FROM documents WHERE id = 'locked'`).Scan(&status, &accessStatus, &generation, &missingCount); err != nil {
		t.Fatal(err)
	}
	if status != "active" || accessStatus != "inaccessible" || generation != 2 || missingCount != 0 {
		t.Fatalf("inaccessible document state = status=%q access=%q generation=%d missing=%d", status, accessStatus, generation, missingCount)
	}
}

func TestBackgroundBacklogCountsEligibleWorkBeyondWorkerQueues(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	add := func(id, extension string) {
		t.Helper()
		doc := Document{
			ID: id, RootID: "root", Path: `X:\` + id + "." + extension,
			NormalizedPath: NormalizePath(`X:\` + id + "." + extension), Name: id + "." + extension,
			Extension: extension, Size: 10, ModifiedAt: modified, LastSeenGeneration: 1,
			Signature: Signature(10, modified),
		}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	add("extractable", "pdf")
	add("binary", "bin")
	if _, err := cat.db.ExecContext(ctx, `UPDATE documents SET ocr_status = 'pending' WHERE id = 'extractable'`); err != nil {
		t.Fatal(err)
	}
	backlog, err := cat.BackgroundBacklog(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Content != 1 || backlog.OCR != 1 || backlog.Hash != 2 {
		t.Fatalf("unexpected backlog: %#v", backlog)
	}
	cat.InvalidateBackgroundBacklog()
	backlog, err = cat.BackgroundBacklog(ctx, map[string][]string{"root": {".pdf"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Content != 0 || backlog.OCR != 1 || backlog.Hash != 2 {
		t.Fatalf("dotted exclusion-aware backlog = %#v", backlog)
	}
}

func TestBackgroundBacklogPolicyExcludesDisabledRootsAndFeatures(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	for _, rootID := range []string{"enabled", "disabled"} {
		doc := Document{ID: rootID, RootID: rootID, Path: `X:\` + rootID + `.pdf`, NormalizedPath: NormalizePath(`X:\` + rootID + `.pdf`), Name: rootID + `.pdf`, Extension: "pdf", Size: 10, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(10, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	policy := BackgroundBacklogPolicy{Strict: true, Content: map[string]BackgroundWorkPolicy{
		"enabled":  {Enabled: true, MaxFileSizeBytes: 100},
		"disabled": {Enabled: false, MaxFileSizeBytes: 100},
	}, OCR: map[string]BackgroundWorkPolicy{}, Hash: map[string]BackgroundWorkPolicy{}}
	backlog, err := cat.BackgroundBacklogWithPolicy(ctx, policy)
	if err != nil {
		t.Fatal(err)
	}
	if backlog.Content != 1 || backlog.OCR != 0 || backlog.Hash != 0 {
		t.Fatalf("policy-filtered backlog = %#v", backlog)
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

func TestFolderActivityPersistsAndRanksFolders(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cat, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.RecordFolderActivity(ctx, "shared", []string{`X:\Active\one.docx`, `X:\Active\two.docx`}, 5, 60*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := cat.RecordFolderActivity(ctx, "shared", []string{`X:\Cold\old.docx`}, 1, 60*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := cat.Close(); err != nil {
		t.Fatal(err)
	}
	cat, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	hot, err := cat.HotFolders(ctx, []string{"shared"}, 60*24*time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hot) != 2 || hot[0].Path != `X:\Active` || hot[0].Score <= hot[1].Score {
		t.Fatalf("unexpected hot folder ranking: %#v", hot)
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

func TestMarkMissingDeletesAfterConsecutiveSuccessfulCrawls(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	doc := Document{ID: "gone", RootID: "test", Path: `D:\fixtures\gone.txt`, NormalizedPath: NormalizePath(`D:\fixtures\gone.txt`), Name: "gone.txt", Extension: "txt", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
	if _, err := cat.UpsertDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	for generation := int64(2); generation <= 3; generation++ {
		if _, err := cat.MarkMissing(ctx, "test", generation, 3); err != nil {
			t.Fatal(err)
		}
		paths, err := cat.MissingDocumentPaths(ctx, "test", 10)
		if err != nil || len(paths) != 1 {
			t.Fatalf("generation %d missing paths = %#v, %v", generation, paths, err)
		}
	}
	if _, err := cat.MarkMissing(ctx, "test", 4, 3); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := cat.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents WHERE root_id = 'test'`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("expired missing document count = %d, want 0", remaining)
	}
	var ftsRows int
	if err := cat.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents_fts`).Scan(&ftsRows); err != nil {
		t.Fatal(err)
	}
	if ftsRows != 0 {
		t.Fatalf("expired document remained in FTS index: %d rows", ftsRows)
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

func TestCompactKeepsContentSearchAndTrimsStoredText(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	modified := time.Now().UTC()
	doc := Document{ID: "large-content", RootID: "test", Path: `D:\fixtures\large.txt`, NormalizedPath: NormalizePath(`D:\fixtures\large.txt`), Name: "large.txt", Extension: "txt", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
	if _, err := cat.UpsertDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("orchid ", 50000)
	if err := cat.UpdateExtractedContent(ctx, doc.ID, doc.Signature, "extracted", content); err != nil {
		t.Fatal(err)
	}
	result, err := cat.Compact(ctx, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if result.TrimmedDocuments != 1 || result.TrimmedTextBytes <= 0 {
		t.Fatalf("unexpected compact result: %#v", result)
	}
	stats, err := cat.StorageStats(ctx, 64<<10)
	if err != nil || stats.OversizedDocuments != 0 || stats.DocumentTextBytes > 64<<10 {
		t.Fatalf("unexpected storage stats: %#v, %v", stats, err)
	}
	resp, err := cat.Search(ctx, SearchRequest{Query: "orchid", Filters: SearchFilters{MatchFields: []string{"content"}}, Limit: 10}, 10)
	if err != nil || len(resp.Results) != 1 || resp.Results[0].ID != doc.ID {
		t.Fatalf("content search after compact = %#v, %v", resp.Results, err)
	}
}

func TestUpdateExtractedContentRepairsInvalidUTF8(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	modified := time.Now().UTC()
	doc := Document{ID: "invalid-utf8", RootID: "test", Path: `D:\fixtures\legacy.txt`, NormalizedPath: NormalizePath(`D:\fixtures\legacy.txt`), Name: "legacy.txt", Extension: "txt", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
	if _, err := cat.UpsertDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	if err := cat.UpdateExtractedContent(ctx, doc.ID, doc.Signature, "extracted", "alpha\x00\x80 beta"); err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := cat.db.QueryRowContext(ctx, `SELECT content_text FROM documents WHERE id = ?`, doc.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(stored) || strings.ContainsRune(stored, '\x00') || stored != "alpha\uFFFD\uFFFD beta" {
		t.Fatalf("stored content = %q, want valid normalized UTF-8", stored)
	}
}

func TestStorageTextCapsPostgresTSVectorInput(t *testing.T) {
	cat := &Catalog{db: &catalogDB{backend: backendPostgres}}
	value := strings.Repeat("distinct-token ", postgresTSVectorContentLimit)
	got := cat.storageText(value)
	if len(got) > postgresTSVectorContentLimit {
		t.Fatalf("PostgreSQL text length = %d, want at most %d", len(got), postgresTSVectorContentLimit)
	}
	if !utf8.ValidString(got) {
		t.Fatal("PostgreSQL text cap split UTF-8")
	}
}

func TestCheckpointSkipsWhileCatalogWriterIsActive(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	cat.writeMu.Lock()
	defer cat.writeMu.Unlock()
	started := time.Now()
	if err := cat.Checkpoint(ctx); err != nil {
		t.Fatalf("checkpoint while writer is active: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("checkpoint waited %s for an active writer", elapsed)
	}
}

func TestCheckpointLeavesPostgresMaintenanceToPostgres(t *testing.T) {
	cat := &Catalog{db: &catalogDB{backend: backendPostgres}}
	if err := cat.Checkpoint(context.Background()); err != nil {
		t.Fatalf("PostgreSQL periodic checkpoint = %v, want no-op", err)
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

func TestClearCrawlHistoryKeepsRootState(t *testing.T) {
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
	removed, err := cat.ClearCrawlHistory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	runs, err := cat.RecentCrawls(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("runs = %#v, want empty", runs)
	}
	states, err := cat.RootStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := states["shared"]; !ok {
		t.Fatal("clearing history removed root state")
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

func TestSearchExactMatchModeSupportsAnySelectedField(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	add := func(id, name, content string) {
		t.Helper()
		doc := Document{ID: id, RootID: "test", Path: `D:\fixtures\` + name, NormalizedPath: NormalizePath(`D:\fixtures\` + name), Name: name, Extension: "txt", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if err := cat.UpdateExtractedContent(ctx, doc.ID, doc.Signature, "extracted", content); err != nil {
			t.Fatal(err)
		}
	}
	add("exact", "contract.txt", "The contract review is ready.")
	add("prefix-only", "contractual.txt", "The contractual review is ready.")
	add("separate-terms", "notes.txt", "The contract legal review is ready.")

	content, err := cat.Search(ctx, SearchRequest{Query: "contract review", Filters: SearchFilters{MatchFields: []string{"content"}, MatchMode: "exact"}, Limit: 10}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(content.Results) != 1 || content.Results[0].ID != "exact" {
		t.Fatalf("exact content search = %#v", content.Results)
	}
	name, err := cat.Search(ctx, SearchRequest{Query: "contract", Filters: SearchFilters{MatchFields: []string{"name"}, MatchMode: "exact"}, Limit: 10}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(name.Results) != 1 || name.Results[0].ID != "exact" {
		t.Fatalf("exact name search = %#v", name.Results)
	}
	if _, err := cat.Search(ctx, SearchRequest{Query: "contract", Filters: SearchFilters{MatchMode: "unknown"}, Limit: 10}, 20); err == nil {
		t.Fatal("expected invalid match mode error")
	}
	boolean, err := cat.Search(ctx, SearchRequest{Filters: SearchFilters{MatchFields: []string{"content"}, MatchMode: "exact", Boolean: BooleanFilter{Any: []string{"contract review", "contractual"}}}, Limit: 10}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(boolean.Results) != 2 || len(boolean.Results[0].MatchedFields) != 1 || boolean.Results[0].MatchedFields[0] != "content" {
		t.Fatalf("exact boolean search = %#v", boolean.Results)
	}
	andNot, err := cat.Search(ctx, SearchRequest{Filters: SearchFilters{MatchFields: []string{"content"}, MatchMode: "exact", Boolean: BooleanFilter{All: []string{"contract", "review"}, Not: []string{"legal"}}}, Limit: 10}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(andNot.Results) != 1 || andNot.Results[0].ID != "exact" {
		t.Fatalf("exact AND/NOT search = %#v", andNot.Results)
	}
	if _, err := cat.Search(ctx, SearchRequest{Filters: SearchFilters{MatchFields: []string{"content"}, Boolean: BooleanFilter{Not: []string{"draft"}}}, Limit: 10}, 20); err == nil {
		t.Fatal("expected invalid boolean filter")
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
	items, err := cat.ClaimPendingContent(ctx, []string{"enabled"}, 10, 1024, nil)
	if err != nil || len(items) != 1 || items[0].RootID != "enabled" {
		t.Fatalf("claimed items = %#v, %v", items, err)
	}
	items, err = cat.ClaimPendingContent(ctx, []string{"disabled"}, 10, 1024, nil)
	if err != nil || len(items) != 1 || items[0].RootID != "disabled" {
		t.Fatalf("claimed items = %#v, %v", items, err)
	}
}

func TestBackgroundFailuresAreBoundedAndSeparatedFromNewWork(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	for _, id := range []string{"transient", "invalid"} {
		doc := Document{ID: id, RootID: "root", Path: `D:\` + id + `.txt`, NormalizedPath: NormalizePath(`D:\` + id + `.txt`), Name: id + ".txt", Extension: "txt", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := cat.ClaimPendingContent(ctx, []string{"root"}, 10, 1024, nil)
	if err != nil || len(claimed) != 2 {
		t.Fatalf("initial claims = %#v, %v", claimed, err)
	}
	if status, err := cat.RecordContentFailure(ctx, "invalid", Signature(1, modified), "invalid or incomplete PDF", false); err != nil || status != "unindexable" {
		t.Fatalf("permanent failure = %q, %v", status, err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		status, err := cat.RecordContentFailure(ctx, "transient", Signature(1, modified), "resource temporarily unavailable", true)
		if err != nil {
			t.Fatal(err)
		}
		want := "retry_pending"
		if attempt == 3 {
			want = "unindexable"
		}
		if status != want {
			t.Fatalf("attempt %d status = %q, want %q", attempt, status, want)
		}
	}
	claimed, err = cat.ClaimPendingContent(ctx, []string{"root"}, 10, 1024, nil)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("terminal failures returned to normal queue: %#v, %v", claimed, err)
	}
}

func TestResetEnrichmentFailuresIsScopedToRequestedRoot(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	for _, rootID := range []string{"manual", "other"} {
		doc := Document{ID: rootID, RootID: rootID, Path: `D:\` + rootID + `.pdf`, NormalizedPath: NormalizePath(`D:\` + rootID + `.pdf`), Name: rootID + ".pdf", Extension: "pdf", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if _, err := cat.RecordContentFailure(ctx, doc.ID, doc.Signature, "access denied", false); err != nil {
			t.Fatal(err)
		}
		if _, err := cat.RecordOCRFailure(ctx, doc.ID, doc.Signature, "access denied", false); err != nil {
			t.Fatal(err)
		}
	}
	content, ocr, err := cat.ResetEnrichmentFailures(ctx, "manual")
	if err != nil || content != 1 || ocr != 1 {
		t.Fatalf("reset = content %d, ocr %d, err %v", content, ocr, err)
	}
	contentJobs, err := cat.ClaimPendingContent(ctx, []string{"manual"}, 10, 1024, nil)
	if err != nil || len(contentJobs) != 1 {
		t.Fatalf("manual content claim = %#v, %v", contentJobs, err)
	}
	ocrJobs, err := cat.ClaimPendingOCR(ctx, []string{"manual"}, 10, 1024, nil)
	if err != nil || len(ocrJobs) != 1 {
		t.Fatalf("manual OCR claim = %#v, %v", ocrJobs, err)
	}
	otherContent, err := cat.ClaimPendingContent(ctx, []string{"other"}, 10, 1024, nil)
	if err != nil || len(otherContent) != 0 {
		t.Fatalf("automatic root requeued terminal work: %#v, %v", otherContent, err)
	}
}

func TestBackgroundClaimsExcludeConfiguredExtensions(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	for _, extension := range []string{"txt", "jpg", "pdf"} {
		id := "file-" + extension
		doc := Document{ID: id, RootID: "root", Path: `D:\\` + id + "." + extension, NormalizedPath: NormalizePath(`D:\\` + id + "." + extension), Name: id + "." + extension, Extension: extension, Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(1, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
		if extension != "txt" {
			if _, err := cat.db.ExecContext(ctx, `UPDATE documents SET ocr_status = 'pending' WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	content, err := cat.ClaimPendingContent(ctx, []string{"root"}, 10, 1024, []string{".jpg"})
	if err != nil || len(content) != 2 {
		t.Fatalf("dotted content exclusion claims = %#v, %v", content, err)
	}
	for _, item := range content {
		if item.Path == `D:\\file-jpg.jpg` {
			t.Fatalf("excluded image was queued for content: %#v", item)
		}
	}
	ocr, err := cat.ClaimPendingOCR(ctx, []string{"root"}, 10, 1024, []string{"jpg"})
	if err != nil || len(ocr) != 1 || ocr[0].Path != `D:\\file-pdf.pdf` {
		t.Fatalf("OCR claims = %#v, %v", ocr, err)
	}
}

func TestRoutePendingContentToOCRPreservesTerminalOCRFailures(t *testing.T) {
	ctx := context.Background()
	cat, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	for _, id := range []string{"pending", "failed"} {
		doc := Document{ID: id, RootID: "root", Path: `C:\` + id + `.pdf`, NormalizedPath: NormalizePath(`C:\` + id + `.pdf`), Name: id + `.pdf`, Extension: "pdf", Size: 10, ModifiedAt: modified, LastSeenGeneration: 1, Signature: Signature(10, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := cat.db.ExecContext(ctx, `UPDATE documents SET ocr_status = 'unindexable' WHERE id = 'failed'`); err != nil {
		t.Fatal(err)
	}
	changed, err := cat.RoutePendingContentToOCR(ctx, "root", []string{".pdf"}, 1024)
	if err != nil || changed != 2 {
		t.Fatalf("route pending PDFs = %d, %v", changed, err)
	}
	rows, err := cat.db.QueryContext(ctx, `SELECT content_status, ocr_status FROM documents WHERE root_id = 'root' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var statuses [][2]string
	for rows.Next() {
		var content, ocr string
		if err := rows.Scan(&content, &ocr); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, [2]string{content, ocr})
	}
	if len(statuses) != 2 || statuses[0] != [2]string{"not_requested", "unindexable"} || statuses[1] != [2]string{"not_requested", "pending"} {
		t.Fatalf("routed statuses = %#v", statuses)
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

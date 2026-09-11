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

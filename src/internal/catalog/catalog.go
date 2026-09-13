package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"qindexer/internal/extract"

	_ "modernc.org/sqlite"
)

type Catalog struct {
	db      *sql.DB
	writeMu sync.Mutex
	statsMu sync.Mutex
	stats   indexStatsCache
}

type indexStatsCache struct {
	at    time.Time
	value IndexStats
}

// FolderActivity is a compact, persisted signal used to allocate a bounded
// filesystem-watch budget. It is deliberately separate from documents.
type FolderActivity struct {
	RootID         string
	Path           string
	Score          float64
	LastActivityAt time.Time
}

type ExtensionCount struct {
	Extension string `json:"extension"`
	Count     int64  `json:"count"`
}

type IndexStats struct {
	Files      int64            `json:"files"`
	Folders    int64            `json:"folders"`
	Types      int64            `json:"types"`
	Extensions []ExtensionCount `json:"extensions"`
}

type Document struct {
	ID                 string              `json:"id"`
	ResultID           string              `json:"result_id"`
	RootID             string              `json:"root_id"`
	Path               string              `json:"path"`
	DisplayPath        string              `json:"display_path,omitempty"`
	NormalizedPath     string              `json:"-"`
	Name               string              `json:"name"`
	Extension          string              `json:"extension"`
	Kind               string              `json:"kind"`
	IsFolder           bool                `json:"is_folder"`
	ETag               string              `json:"etag"`
	Size               int64               `json:"size"`
	ModifiedAt         time.Time           `json:"modified_at"`
	CreatedAt          time.Time           `json:"created_at,omitempty"`
	Status             string              `json:"status"`
	LastSeenGeneration int64               `json:"-"`
	LastIndexedAt      time.Time           `json:"indexed_at"`
	ContentStatus      string              `json:"content_status,omitempty"`
	OCRStatus          string              `json:"ocr_status,omitempty"`
	ContentText        string              `json:"-"`
	ContentHash        string              `json:"content_hash,omitempty"`
	HashStatus         string              `json:"hash_status,omitempty"`
	Owner              string              `json:"owner,omitempty"`
	AccessStatus       string              `json:"access_status"`
	MovedFromPath      string              `json:"moved_from_path,omitempty"`
	Signature          string              `json:"-"`
	Score              float64             `json:"score,omitempty"`
	MatchedFields      []string            `json:"matched_fields,omitempty"`
	Highlights         map[string][]string `json:"highlights,omitempty"`
}

type RootState struct {
	RootID                  string     `json:"root_id"`
	LastGeneration          int64      `json:"last_generation"`
	LastSuccessfulCrawlAt   *time.Time `json:"last_successful_crawl_at,omitempty"`
	LastCrawlStatus         string     `json:"last_crawl_status"`
	LastError               string     `json:"last_error,omitempty"`
	DocumentCount           int64      `json:"document_count"`
	MissingCount            int64      `json:"missing_count"`
	ConsecutiveMissingLimit int        `json:"-"`
}

type CrawlRun struct {
	ID             string     `json:"id"`
	RootID         string     `json:"root_id"`
	Generation     int64      `json:"generation"`
	StartedAt      time.Time  `json:"started_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Status         string     `json:"status"`
	FilesSeen      int64      `json:"files_seen"`
	FilesAdded     int64      `json:"files_added"`
	FilesUpdated   int64      `json:"files_updated"`
	FilesUnchanged int64      `json:"files_unchanged"`
	FilesMissing   int64      `json:"files_missing"`
	Errors         int64      `json:"errors"`
	ErrorMessage   string     `json:"error_message,omitempty"`
}

type UpsertResult struct {
	Added     bool
	Updated   bool
	Unchanged bool
}

type BackgroundCandidate struct {
	ID        string
	RootID    string
	Path      string
	Signature string
	Size      int64
}

type SearchRequest struct {
	SearchID      string        `json:"search_id,omitempty"`
	Query         string        `json:"query"`
	Filters       SearchFilters `json:"filters"`
	Limit         int           `json:"limit"`
	Offset        int           `json:"offset"`
	Sort          string        `json:"sort"`
	SortDirection string        `json:"sort_direction"`
	Direction     string        `json:"direction"`
	Cursor        string        `json:"cursor,omitempty"`
}

type SearchFilters struct {
	Roots          []string      `json:"roots"`
	Extensions     []string      `json:"extensions"`
	PathPrefix     string        `json:"path_prefix"`
	PathPrefixes   []string      `json:"path_prefixes"`
	IncludePaths   []string      `json:"include_paths"`
	ExcludePaths   []string      `json:"exclude_paths"`
	ScopeAliases   []ScopeAlias  `json:"scope_aliases"`
	Kind           string        `json:"kind"`
	ModifiedAfter  string        `json:"modified_after"`
	ModifiedBefore string        `json:"modified_before"`
	MinSize        *int64        `json:"min_size"`
	MaxSize        *int64        `json:"max_size"`
	MatchFields    []string      `json:"match_fields"`
	MatchMode      string        `json:"match_mode"`
	Boolean        BooleanFilter `json:"boolean"`
}

// BooleanFilter is a structured expression, deliberately avoiding raw FTS
// operators in client-supplied query text. all uses AND, any uses OR, and not
// excludes matching terms from the positive expression.
type BooleanFilter struct {
	All []string `json:"all,omitempty"`
	Any []string `json:"any,omitempty"`
	Not []string `json:"not,omitempty"`
}

func (f BooleanFilter) TermCount() int { return len(f.All) + len(f.Any) + len(f.Not) }

// ScopeAlias is an ephemeral client path relationship used only to resolve a
// request's explicit scope fields. It is never persisted in QIndexer config.
type ScopeAlias struct {
	Path     string `json:"path"`
	Target   string `json:"target"`
	Platform string `json:"platform,omitempty"`
}

type SearchResponse struct {
	SearchID   string          `json:"search_id"`
	Results    []Document      `json:"results"`
	Offset     int             `json:"offset"`
	HasMore    bool            `json:"has_more"`
	NextOffset *int            `json:"next_offset,omitempty"`
	NextCursor string          `json:"next_cursor,omitempty"`
	TookMS     int64           `json:"took_ms"`
	Index      SearchIndexInfo `json:"index"`
}

type SearchIndexInfo struct {
	Generation       int64              `json:"generation"`
	FreshnessSeconds *int64             `json:"freshness_seconds,omitempty"`
	Roots            []SearchRootStatus `json:"roots"`
}

type SearchRootStatus struct {
	RootID                string     `json:"root_id"`
	Generation            int64      `json:"generation"`
	LastStatus            string     `json:"last_status"`
	LastSuccessfulCrawlAt *time.Time `json:"last_successful_crawl_at,omitempty"`
	FreshnessSeconds      *int64     `json:"freshness_seconds,omitempty"`
}

type RequestError struct {
	Code    string
	Message string
}

func (e *RequestError) Error() string { return e.Message }

func Open(ctx context.Context, dataDir string) (*Catalog, error) {
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dataDir, 0700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "qsurfer-search.db")
	// Apply these settings to every pooled connection. Setting them only during
	// migration leaves later reader/writer connections with SQLite's zero busy
	// timeout, which turns ordinary writer contention into SQLITE_BUSY errors.
	dsn := "file:" + filepath.ToSlash(dbPath) + "?_pragma=busy_timeout(30000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// WAL supports concurrent readers; a bounded pool prevents one slow client from serializing every search.
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	c := &Catalog{db: db}
	if err := c.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return c, nil
}

func (c *Catalog) Close() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, _ = c.db.Exec(`PRAGMA optimize;`)
	// A passive checkpoint never blocks shutdown behind an active reader. WAL
	// recovery is automatic on the next open, so do not force a truncate while
	// a stop or repair is still unwinding.
	_, _ = c.db.Exec(`PRAGMA wal_checkpoint(PASSIVE);`)
	return c.db.Close()
}

// CheckDatabase performs a read-only SQLite quick check. It is deliberately
// separate from Open so an operator can inspect a backup without creating WAL
// sidecars or mutating the source database.
func CheckDatabase(ctx context.Context, path string) error {
	// modernc SQLite accepts the Windows-friendly file:C:/... URI form. Using
	// net/url produces file:///C:/..., which this driver misparses on Windows.
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var schemaObjects int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master`).Scan(&schemaObjects); err != nil {
		return fmt.Errorf("SQLite schema read failed: %w", err)
	}
	var documents int64
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents`).Scan(&documents); err != nil {
		return fmt.Errorf("SQLite documents read failed: %w", err)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return fmt.Errorf("SQLite quick check failed after reading %d schema objects and %d documents: %w", schemaObjects, documents, err)
	}
	defer rows.Close()
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		if result != "ok" {
			return fmt.Errorf("SQLite integrity check failed: %s", result)
		}
	}
	return rows.Err()
}

// ClearRoot removes only catalog state belonging to one configured root.
func (c *Catalog) ClearRoot(ctx context.Context, rootID string) (int64, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE root_id = ?`, rootID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM documents_fts WHERE root_id = ?`, rootID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM crawl_checkpoints WHERE root_id = ?`, rootID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM crawl_runs WHERE root_id = ?`, rootID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM root_states WHERE root_id = ?`, rootID); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM folder_activity WHERE root_id = ?`, rootID); err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return count, tx.Commit()
}

func (c *Catalog) DeactivateRoot(ctx context.Context, rootID string) (int64, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO deleted_roots(root_id, deleted_at) VALUES (?, ?)`, rootID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM folder_activity WHERE root_id = ?`, rootID); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE documents SET status = 'deleted', access_status = 'root_removed' WHERE root_id = ? AND status <> 'deleted'`, rootID)
	if err != nil {
		return 0, err
	}
	count, _ := result.RowsAffected()
	return count, tx.Commit()
}

func (c *Catalog) RewriteRootPath(ctx context.Context, rootID, oldRootPath, newRootPath string) (int64, int64, error) {
	return c.rewriteRootPath(ctx, rootID, oldRootPath, newRootPath, nil)
}

// RewriteRootPathWithProgress reports candidate discovery and completed path
// operations while a large root migration is in progress.
func (c *Catalog) RewriteRootPathWithProgress(ctx context.Context, rootID, oldRootPath, newRootPath string, progress func(matched, processed int64)) (int64, int64, error) {
	return c.rewriteRootPath(ctx, rootID, oldRootPath, newRootPath, progress)
}

func (c *Catalog) rewriteRootPath(ctx context.Context, rootID, oldRootPath, newRootPath string, progress func(matched, processed int64)) (int64, int64, error) {
	return c.rewriteRootPathBatched(ctx, rootID, oldRootPath, newRootPath, progress)
}

// rewriteRootPathBatched keeps each migration commit small. A repair can touch
// hundreds of thousands of rows, so one transaction would block every crawler
// writer and is needlessly vulnerable to interruption.
func (c *Catalog) rewriteRootPathBatched(ctx context.Context, rootID, oldRootPath, newRootPath string, progress func(matched, processed int64)) (int64, int64, error) {
	oldRootPath = strings.TrimSpace(oldRootPath)
	newRootPath = strings.TrimSpace(newRootPath)
	if rootPathSame(oldRootPath, newRootPath) {
		return 0, 0, nil
	}
	var matched int64
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents
		WHERE root_id = ? AND (normalized_path = ? OR normalized_path LIKE ? ESCAPE '\')`,
		rootID, NormalizePath(oldRootPath), escapeLike(pathChildPrefix(oldRootPath))+"%").Scan(&matched); err != nil {
		return 0, 0, err
	}
	if progress != nil {
		progress(matched, 0)
	}
	var processed, rewritten, merged int64
	for {
		batchProcessed, batchRewritten, batchMerged, err := c.rewriteRootPathBatch(ctx, rootID, oldRootPath, newRootPath, 128)
		if err != nil {
			return rewritten, merged, err
		}
		if batchProcessed == 0 {
			break
		}
		processed += batchProcessed
		rewritten += batchRewritten
		merged += batchMerged
		if progress != nil {
			progress(matched, processed)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := c.ClearRootCheckpoints(ctx, rootID); err != nil {
		return rewritten, merged, err
	}
	return rewritten, merged, nil
}

func (c *Catalog) rewriteRootPathBatch(ctx context.Context, rootID, oldRootPath, newRootPath string, limit int) (int64, int64, int64, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	defer tx.Rollback()
	type candidate struct{ id, path, extension, content string }
	rows, err := tx.QueryContext(ctx, `SELECT id, path, extension, content_text FROM documents
		WHERE root_id = ? AND (normalized_path = ? OR normalized_path LIKE ? ESCAPE '\') LIMIT ?`,
		rootID, NormalizePath(oldRootPath), escapeLike(pathChildPrefix(oldRootPath))+"%", limit)
	if err != nil {
		return 0, 0, 0, err
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.id, &item.path, &item.extension, &item.content); err != nil {
			rows.Close()
			return 0, 0, 0, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return 0, 0, 0, err
	}
	var rewritten, merged int64
	for _, item := range candidates {
		suffix, ok := rootPathSuffix(item.path, oldRootPath)
		if !ok {
			return 0, 0, 0, fmt.Errorf("repair candidate is not below its source alias")
		}
		newPath := joinRootPath(newRootPath, suffix)
		newNormalized := NormalizePath(newPath)
		var existingID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM documents WHERE root_id = ? AND normalized_path = ?`, rootID, newNormalized).Scan(&existingID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, 0, 0, err
		}
		if existingID != "" && existingID != item.id {
			if _, err := tx.ExecContext(ctx, `DELETE FROM documents_fts WHERE id = ?`, item.id); err != nil {
				return 0, 0, 0, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE id = ?`, item.id); err != nil {
				return 0, 0, 0, err
			}
			merged++
			continue
		}
		name := filepath.Base(newPath)
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET path = ?, normalized_path = ?, name = ? WHERE id = ?`, newPath, newNormalized, name, item.id); err != nil {
			return 0, 0, 0, err
		}
		if err := upsertFTS(ctx, tx, item.id, rootID, name, newPath, item.extension, item.content); err != nil {
			return 0, 0, 0, err
		}
		rewritten++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, err
	}
	return int64(len(candidates)), rewritten, merged, nil
}

// rewriteRootPathLegacy preserves the previous single-transaction
// implementation as a reference while repairs use the resumable batched path.
func (c *Catalog) rewriteRootPathLegacy(ctx context.Context, rootID, oldRootPath, newRootPath string, progress func(matched, processed int64)) (int64, int64, error) {
	oldRootPath = strings.TrimSpace(oldRootPath)
	newRootPath = strings.TrimSpace(newRootPath)
	if rootPathSame(oldRootPath, newRootPath) {
		return 0, 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	// Only inspect rows under the source alias. A repair commonly runs after a
	// successful migration, so scanning every document in a large root just to
	// discover there are no X:\\ paths left is needlessly expensive.
	rows, err := tx.QueryContext(ctx, `SELECT id, path, normalized_path, extension
		FROM documents
		WHERE root_id = ? AND (normalized_path = ? OR normalized_path LIKE ? ESCAPE '\')`,
		rootID, NormalizePath(oldRootPath), escapeLike(pathChildPrefix(oldRootPath))+"%")
	if err != nil {
		return 0, 0, err
	}
	type rewriteCandidate struct {
		id, path, normalizedPath, extension string
		suffix                              string
	}
	candidates := []rewriteCandidate{}
	for rows.Next() {
		var candidate rewriteCandidate
		if err := rows.Scan(&candidate.id, &candidate.path, &candidate.normalizedPath, &candidate.extension); err != nil {
			rows.Close()
			return 0, 0, err
		}
		suffix, ok := rootPathSuffix(candidate.path, oldRootPath)
		if !ok {
			continue
		}
		candidate.suffix = suffix
		candidates = append(candidates, candidate)
	}
	if err := rows.Close(); err != nil {
		return 0, 0, err
	}
	if progress != nil {
		progress(int64(len(candidates)), 0)
	}
	var rewritten, merged int64
	for i, candidate := range candidates {
		newPath := joinRootPath(newRootPath, candidate.suffix)
		newNormalized := NormalizePath(newPath)
		var existingID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM documents WHERE root_id = ? AND normalized_path = ?`, rootID, newNormalized).Scan(&existingID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, 0, err
		}
		if existingID != "" && existingID != candidate.id {
			if _, err := tx.ExecContext(ctx, `DELETE FROM documents_fts WHERE id = ?`, candidate.id); err != nil {
				return 0, 0, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE id = ?`, candidate.id); err != nil {
				return 0, 0, err
			}
			merged++
		} else {
			name := filepath.Base(newPath)
			var content string
			if err := tx.QueryRowContext(ctx, `SELECT content_text FROM documents WHERE id = ?`, candidate.id).Scan(&content); err != nil {
				return 0, 0, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE documents SET path = ?, normalized_path = ?, name = ? WHERE id = ?`, newPath, newNormalized, name, candidate.id); err != nil {
				return 0, 0, err
			}
			if err := upsertFTS(ctx, tx, candidate.id, rootID, name, newPath, candidate.extension, content); err != nil {
				return 0, 0, err
			}
			rewritten++
		}
		if progress != nil && ((i+1)%100 == 0 || i+1 == len(candidates)) {
			progress(int64(len(candidates)), int64(i+1))
		}
	}
	checkpoints, err := tx.QueryContext(ctx, `SELECT path FROM crawl_checkpoints
		WHERE root_id = ? AND (normalized_path = ? OR normalized_path LIKE ? ESCAPE '\')`,
		rootID, NormalizePath(oldRootPath), escapeLike(pathChildPrefix(oldRootPath))+"%")
	if err != nil {
		return 0, 0, err
	}
	checkpointPaths := []string{}
	for checkpoints.Next() {
		var path string
		if err := checkpoints.Scan(&path); err != nil {
			checkpoints.Close()
			return 0, 0, err
		}
		checkpointPaths = append(checkpointPaths, path)
	}
	if err := checkpoints.Close(); err != nil {
		return 0, 0, err
	}
	for _, path := range checkpointPaths {
		suffix, ok := rootPathSuffix(path, oldRootPath)
		if !ok {
			continue
		}
		newPath := joinRootPath(newRootPath, suffix)
		if _, err := tx.ExecContext(ctx, `DELETE FROM crawl_checkpoints WHERE root_id = ? AND normalized_path = ?`, rootID, NormalizePath(path)); err != nil {
			return 0, 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO crawl_checkpoints(root_id, normalized_path, path, completed_at) VALUES (?, ?, ?, ?)`,
			rootID, NormalizePath(newPath), newPath, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return 0, 0, err
		}
	}
	return rewritten, merged, tx.Commit()
}

func (c *Catalog) RepairEmbeddedRootPath(ctx context.Context, rootID, canonicalRootPath string) (int64, int64, error) {
	canonicalRootPath = strings.TrimSpace(canonicalRootPath)
	if canonicalRootPath == "" {
		return 0, 0, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	patterns := embeddedRootPathLikePatterns(canonicalRootPath)
	if len(patterns) == 0 {
		return 0, 0, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, path, normalized_path, extension, content_text
		FROM documents
		WHERE root_id = ? AND (normalized_path LIKE ? ESCAPE '\' OR normalized_path LIKE ? ESCAPE '\')`,
		rootID, patterns[0], patterns[1])
	if err != nil {
		return 0, 0, err
	}
	type repairCandidate struct {
		id, path, normalizedPath, extension, content string
		suffix                                       string
	}
	candidates := []repairCandidate{}
	for rows.Next() {
		var candidate repairCandidate
		if err := rows.Scan(&candidate.id, &candidate.path, &candidate.normalizedPath, &candidate.extension, &candidate.content); err != nil {
			rows.Close()
			return 0, 0, err
		}
		suffix, ok := embeddedRootPathSuffix(candidate.path, canonicalRootPath)
		if !ok {
			continue
		}
		candidate.suffix = suffix
		candidates = append(candidates, candidate)
	}
	if err := rows.Close(); err != nil {
		return 0, 0, err
	}
	var rewritten, merged int64
	for _, candidate := range candidates {
		newPath := joinRootPath(canonicalRootPath, candidate.suffix)
		newNormalized := NormalizePath(newPath)
		var existingID string
		err := tx.QueryRowContext(ctx, `SELECT id FROM documents WHERE root_id = ? AND normalized_path = ?`, rootID, newNormalized).Scan(&existingID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, 0, err
		}
		if existingID != "" && existingID != candidate.id {
			if _, err := tx.ExecContext(ctx, `DELETE FROM documents_fts WHERE id = ?`, candidate.id); err != nil {
				return 0, 0, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE id = ?`, candidate.id); err != nil {
				return 0, 0, err
			}
			merged++
			continue
		}
		name := filepath.Base(newPath)
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET path = ?, normalized_path = ?, name = ? WHERE id = ?`, newPath, newNormalized, name, candidate.id); err != nil {
			return 0, 0, err
		}
		if err := upsertFTS(ctx, tx, candidate.id, rootID, name, newPath, candidate.extension, candidate.content); err != nil {
			return 0, 0, err
		}
		rewritten++
	}
	checkpoints, err := tx.QueryContext(ctx, `SELECT path FROM crawl_checkpoints
		WHERE root_id = ? AND (normalized_path LIKE ? ESCAPE '\' OR normalized_path LIKE ? ESCAPE '\')`,
		rootID, patterns[0], patterns[1])
	if err != nil {
		return 0, 0, err
	}
	checkpointPaths := []string{}
	for checkpoints.Next() {
		var path string
		if err := checkpoints.Scan(&path); err != nil {
			checkpoints.Close()
			return 0, 0, err
		}
		checkpointPaths = append(checkpointPaths, path)
	}
	if err := checkpoints.Close(); err != nil {
		return 0, 0, err
	}
	for _, path := range checkpointPaths {
		suffix, ok := embeddedRootPathSuffix(path, canonicalRootPath)
		if !ok {
			continue
		}
		newPath := joinRootPath(canonicalRootPath, suffix)
		if _, err := tx.ExecContext(ctx, `DELETE FROM crawl_checkpoints WHERE root_id = ? AND normalized_path = ?`, rootID, NormalizePath(path)); err != nil {
			return 0, 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO crawl_checkpoints(root_id, normalized_path, path, completed_at) VALUES (?, ?, ?, ?)`,
			rootID, NormalizePath(newPath), newPath, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return 0, 0, err
		}
	}
	return rewritten, merged, tx.Commit()
}

func (c *Catalog) PruneRecoveryPaths(ctx context.Context, rootID string) (int64, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	patterns := []string{"%@recently-snapshot%", "%@recycle%", "%#recycle%", "%$recycle.bin%", "%recycler%", "%.sync%", "%.qsync%", "%.qsync_sn%"}
	clauses := make([]string, len(patterns))
	args := []any{rootID}
	for i, pattern := range patterns {
		clauses[i] = "normalized_path LIKE ?"
		args = append(args, pattern)
	}
	where := "root_id = ? AND (" + strings.Join(clauses, " OR ") + ")"
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM documents_fts WHERE id IN (SELECT id FROM documents WHERE `+where+`)`, args...); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM documents WHERE `+where, args...)
	if err != nil {
		return 0, err
	}
	count, _ := result.RowsAffected()
	return count, tx.Commit()
}

// RecordFolderActivity keeps a small, decaying signal for the watcher. Paths
// are reduced to their containing directory so one busy folder is one row.
func (c *Catalog) RecordFolderActivity(ctx context.Context, rootID string, paths []string, weight float64, halfLife time.Duration) error {
	if strings.TrimSpace(rootID) == "" || len(paths) == 0 || weight <= 0 {
		return nil
	}
	if halfLife <= 0 {
		halfLife = 60 * 24 * time.Hour
	}
	now := time.Now().UTC()
	directories := map[string]string{}
	for _, path := range paths {
		dir := filepath.Dir(path)
		if strings.TrimSpace(dir) == "" || dir == "." {
			continue
		}
		directories[NormalizePath(dir)] = dir
	}
	if len(directories) == 0 {
		return nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for normalized, path := range directories {
		var score float64
		var recorded string
		err := tx.QueryRowContext(ctx, `SELECT score, last_activity_at FROM folder_activity WHERE root_id = ? AND normalized_path = ?`, rootID, normalized).Scan(&score, &recorded)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			if at, parseErr := time.Parse(time.RFC3339Nano, recorded); parseErr == nil {
				score = decayActivity(score, now.Sub(at), halfLife)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO folder_activity(root_id, normalized_path, path, score, last_activity_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(root_id, normalized_path) DO UPDATE SET path = excluded.path, score = excluded.score, last_activity_at = excluded.last_activity_at`,
			rootID, normalized, path, score+weight, now.Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (c *Catalog) HotFolders(ctx context.Context, rootIDs []string, halfLife time.Duration, limit int) ([]FolderActivity, error) {
	if len(rootIDs) == 0 || limit <= 0 {
		return nil, nil
	}
	if halfLife <= 0 {
		halfLife = 60 * 24 * time.Hour
	}
	rows, err := c.db.QueryContext(ctx, `SELECT root_id, path, score, last_activity_at FROM folder_activity WHERE root_id IN (`+placeholders(len(rootIDs))+`)`, stringsToAny(rootIDs)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now().UTC()
	values := make([]FolderActivity, 0)
	for rows.Next() {
		var item FolderActivity
		var recorded string
		if err := rows.Scan(&item.RootID, &item.Path, &item.Score, &recorded); err != nil {
			return nil, err
		}
		item.LastActivityAt, _ = time.Parse(time.RFC3339Nano, recorded)
		item.Score = decayActivity(item.Score, now.Sub(item.LastActivityAt), halfLife)
		if item.Score > 0 {
			values = append(values, item)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(values, func(i, j int) bool { return values[i].Score > values[j].Score })
	if len(values) > limit {
		values = values[:limit]
	}
	return values, nil
}

func (c *Catalog) IndexStats(ctx context.Context) (IndexStats, error) {
	c.statsMu.Lock()
	defer c.statsMu.Unlock()
	if time.Since(c.stats.at) < 5*time.Second {
		return c.stats.value, nil
	}
	var stats IndexStats
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents WHERE status = 'active' AND is_folder = 0`).Scan(&stats.Files); err != nil {
		return IndexStats{}, err
	}
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents WHERE status = 'active' AND is_folder = 1`).Scan(&stats.Folders); err != nil {
		return IndexStats{}, err
	}
	if err := c.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT extension) FROM documents WHERE status = 'active' AND is_folder = 0`).Scan(&stats.Types); err != nil {
		return IndexStats{}, err
	}
	rows, err := c.db.QueryContext(ctx, `SELECT extension, COUNT(*) FROM documents WHERE status = 'active' AND is_folder = 0 GROUP BY extension ORDER BY COUNT(*) DESC, extension ASC LIMIT 8`)
	if err != nil {
		return IndexStats{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var item ExtensionCount
		if err := rows.Scan(&item.Extension, &item.Count); err != nil {
			return IndexStats{}, err
		}
		stats.Extensions = append(stats.Extensions, item)
	}
	if err := rows.Err(); err != nil {
		return IndexStats{}, err
	}
	c.stats = indexStatsCache{at: time.Now(), value: stats}
	return stats, nil
}

func decayActivity(score float64, elapsed, halfLife time.Duration) float64 {
	if score <= 0 || elapsed <= 0 || halfLife <= 0 {
		return score
	}
	return score * math.Pow(0.5, elapsed.Seconds()/halfLife.Seconds())
}

func stringsToAny(values []string) []any {
	args := make([]any, len(values))
	for i := range values {
		args[i] = values[i]
	}
	return args
}

func (c *Catalog) migrate(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA synchronous=FULL;`,
		`PRAGMA busy_timeout=30000;`,
		`CREATE TABLE IF NOT EXISTS documents (
			id TEXT PRIMARY KEY,
			root_id TEXT NOT NULL,
			path TEXT NOT NULL,
			normalized_path TEXT NOT NULL,
			name TEXT NOT NULL,
			extension TEXT NOT NULL,
			size INTEGER NOT NULL,
			modified_at TEXT NOT NULL,
			created_at TEXT,
			status TEXT NOT NULL,
			last_seen_generation INTEGER NOT NULL,
			last_indexed_at TEXT NOT NULL,
			content_status TEXT NOT NULL,
			ocr_status TEXT NOT NULL DEFAULT 'not_requested',
			content_text TEXT NOT NULL DEFAULT '',
			content_hash TEXT NOT NULL DEFAULT '',
			hash_status TEXT NOT NULL DEFAULT 'not_hashed',
			owner TEXT NOT NULL DEFAULT '',
			access_status TEXT NOT NULL DEFAULT 'metadata_readable',
			moved_from_path TEXT NOT NULL DEFAULT '',
			is_folder INTEGER NOT NULL DEFAULT 0,
			signature TEXT NOT NULL,
			missing_count INTEGER NOT NULL DEFAULT 0,
			UNIQUE(root_id, normalized_path)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_documents_root_status ON documents(root_id, status);`,
		`CREATE INDEX IF NOT EXISTS idx_documents_extension ON documents(extension);`,
		`CREATE INDEX IF NOT EXISTS idx_documents_modified ON documents(modified_at);`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS documents_fts USING fts5(
			id UNINDEXED,
			root_id UNINDEXED,
			name,
			path,
			extension
		);`,
		`CREATE TABLE IF NOT EXISTS root_states (
			root_id TEXT PRIMARY KEY,
			last_generation INTEGER NOT NULL DEFAULT 0,
			last_successful_crawl_at TEXT,
			last_crawl_status TEXT NOT NULL DEFAULT 'never',
			last_error TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS crawl_runs (
			id TEXT PRIMARY KEY,
			root_id TEXT NOT NULL,
			generation INTEGER NOT NULL,
			started_at TEXT NOT NULL,
			finished_at TEXT,
			status TEXT NOT NULL,
			files_seen INTEGER NOT NULL DEFAULT 0,
			files_added INTEGER NOT NULL DEFAULT 0,
			files_updated INTEGER NOT NULL DEFAULT 0,
			files_unchanged INTEGER NOT NULL DEFAULT 0,
			files_missing INTEGER NOT NULL DEFAULT 0,
			errors INTEGER NOT NULL DEFAULT 0,
			error_message TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS crawl_checkpoints (
			root_id TEXT NOT NULL,
			normalized_path TEXT NOT NULL,
			path TEXT NOT NULL,
			completed_at TEXT NOT NULL,
			PRIMARY KEY(root_id, normalized_path)
		);`,
		`CREATE TABLE IF NOT EXISTS deleted_roots (root_id TEXT PRIMARY KEY, deleted_at TEXT NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS folder_activity (
			root_id TEXT NOT NULL,
			normalized_path TEXT NOT NULL,
			path TEXT NOT NULL,
			score REAL NOT NULL,
			last_activity_at TEXT NOT NULL,
			PRIMARY KEY(root_id, normalized_path)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_folder_activity_root_score ON folder_activity(root_id, score DESC);`,
	}
	for _, stmt := range stmts {
		if _, err := c.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if err := c.ensureColumn(ctx, "documents", "is_folder", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := c.ensureColumn(ctx, "documents", "owner", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := c.ensureColumn(ctx, "documents", "access_status", "TEXT NOT NULL DEFAULT 'metadata_readable'"); err != nil {
		return err
	}
	if err := c.ensureColumn(ctx, "documents", "moved_from_path", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := c.ensureColumn(ctx, "documents", "content_text", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := c.ensureColumn(ctx, "documents", "ocr_status", "TEXT NOT NULL DEFAULT 'not_requested'"); err != nil {
		return err
	}
	if err := c.ensureColumn(ctx, "documents", "content_hash", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := c.ensureColumn(ctx, "documents", "hash_status", "TEXT NOT NULL DEFAULT 'not_hashed'"); err != nil {
		return err
	}
	if err := c.ensureContentFTS(ctx); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE documents SET content_status = 'not_indexed' WHERE content_status = 'queued'`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE documents SET ocr_status = 'pending' WHERE ocr_status = 'queued'`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `UPDATE documents SET hash_status = 'not_hashed' WHERE hash_status = 'queued'`); err != nil {
		return err
	}
	_, err := c.db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_documents_status_kind_path ON documents(status, is_folder, normalized_path)`)
	return err
}

func (c *Catalog) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := c.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition)
	return err
}

func (c *Catalog) ensureContentFTS(ctx context.Context) error {
	var sqlText string
	err := c.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'documents_fts'`).Scan(&sqlText)
	if err != nil {
		return err
	}
	if strings.Contains(strings.ToLower(sqlText), "content") {
		return nil
	}
	if _, err := c.db.ExecContext(ctx, `DROP TABLE documents_fts`); err != nil {
		return err
	}
	if _, err := c.db.ExecContext(ctx, `CREATE VIRTUAL TABLE documents_fts USING fts5(id UNINDEXED, root_id UNINDEXED, name, path, extension, content)`); err != nil {
		return err
	}
	_, err = c.db.ExecContext(ctx, `INSERT INTO documents_fts(id, root_id, name, path, extension, content) SELECT id, root_id, name, path, extension, content_text FROM documents`)
	return err
}

func NormalizePath(path string) string {
	cleaned := filepath.Clean(path)
	if runtime.GOOS == "windows" || isWindowsRootPath(cleaned) {
		return strings.ToLower(cleaned)
	}
	return cleaned
}

func rootPathSuffix(path, root string) (string, bool) {
	windowsStyle := isWindowsRootPath(path) || isWindowsRootPath(root)
	path = strings.ReplaceAll(filepath.Clean(path), "\\", "/")
	root = strings.TrimRight(strings.ReplaceAll(filepath.Clean(root), "\\", "/"), "/")
	if root == "." || root == "" {
		return "", false
	}
	if rootPathEqual(path, root, windowsStyle) {
		return "", true
	}
	if len(path) > len(root) && rootPathEqual(path[:len(root)], root, windowsStyle) && path[len(root)] == '/' {
		return path[len(root)+1:], true
	}
	return "", false
}

func embeddedRootPathSuffix(path, root string) (string, bool) {
	pathParts := normalizedPathParts(path)
	rootParts := normalizedPathParts(root)
	if len(pathParts) == 0 || len(rootParts) == 0 {
		return "", false
	}
	if len(pathParts) == len(rootParts) && pathPartsEqual(pathParts, rootParts) {
		return "", false
	}
	for start := 1; start+len(rootParts) <= len(pathParts); start++ {
		if pathPartsEqual(pathParts[start:start+len(rootParts)], rootParts) {
			return strings.Join(pathParts[start+len(rootParts):], "/"), true
		}
	}
	return "", false
}

// embeddedRootPathLikePatterns finds only paths where the canonical root is
// nested below another path. The leading '_' keeps ordinary canonical paths
// out of the candidate set; embeddedRootPathSuffix then verifies the exact
// segment boundary before any rewrite occurs.
func embeddedRootPathLikePatterns(root string) []string {
	parts := normalizedPathParts(root)
	if len(parts) == 0 {
		return nil
	}
	return []string{
		"_%" + escapeLike("/"+strings.Join(parts, "/")) + "%",
		"_%" + escapeLike("\\"+strings.Join(parts, "\\")) + "%",
	}
}

func normalizedPathParts(value string) []string {
	value = strings.ReplaceAll(strings.TrimSpace(value), "\\", "/")
	raw := strings.Split(value, "/")
	parts := make([]string, 0, len(raw))
	for _, part := range raw {
		part = strings.TrimSpace(part)
		if part == "" || part == "." {
			continue
		}
		parts = append(parts, part)
	}
	return parts
}

func pathPartsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

func joinRootPath(root, suffix string) string {
	if suffix == "" {
		return root
	}
	if isWindowsRootPath(root) {
		return strings.TrimRight(root, "\\/") + "\\" + strings.ReplaceAll(suffix, "/", "\\")
	}
	return strings.TrimRight(root, "\\/") + "/" + strings.ReplaceAll(suffix, "\\", "/")
}

func rootPathSame(a, b string) bool {
	windowsStyle := isWindowsRootPath(a) || isWindowsRootPath(b)
	return rootPathEqual(strings.TrimRight(strings.ReplaceAll(filepath.Clean(a), "\\", "/"), "/"), strings.TrimRight(strings.ReplaceAll(filepath.Clean(b), "\\", "/"), "/"), windowsStyle)
}

func isWindowsRootPath(value string) bool {
	value = strings.TrimSpace(value)
	return strings.Contains(value, "\\") || (len(value) >= 2 && value[1] == ':')
}

func rootPathEqual(a, b string, windowsStyle bool) bool {
	if windowsStyle {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func Signature(size int64, modifiedAt time.Time) string {
	return fmt.Sprintf("%d:%s", size, modifiedAt.UTC().Format(time.RFC3339Nano))
}

func (c *Catalog) NextGeneration(ctx context.Context, rootID string) (int64, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.db.ExecContext(ctx, `INSERT OR IGNORE INTO root_states(root_id) VALUES (?)`, rootID)
	if err != nil {
		return 0, err
	}
	var generation int64
	if err := c.db.QueryRowContext(ctx, `SELECT last_generation + 1 FROM root_states WHERE root_id = ?`, rootID).Scan(&generation); err != nil {
		return 0, err
	}
	_, err = c.db.ExecContext(ctx, `UPDATE root_states SET last_generation = ?, last_crawl_status = 'running', last_error = '' WHERE root_id = ?`, generation, rootID)
	return generation, err
}

func (c *Catalog) StartCrawl(ctx context.Context, run CrawlRun) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.db.ExecContext(ctx, `INSERT INTO crawl_runs(id, root_id, generation, started_at, status) VALUES (?, ?, ?, ?, ?)`,
		run.ID, run.RootID, run.Generation, run.StartedAt.UTC().Format(time.RFC3339Nano), run.Status)
	return err
}

func (c *Catalog) FinishCrawl(ctx context.Context, run CrawlRun) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	finished := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE crawl_runs
		SET finished_at = ?, status = ?, files_seen = ?, files_added = ?, files_updated = ?, files_unchanged = ?, files_missing = ?, errors = ?, error_message = ?
		WHERE id = ?`,
		finished, run.Status, run.FilesSeen, run.FilesAdded, run.FilesUpdated, run.FilesUnchanged, run.FilesMissing, run.Errors, run.ErrorMessage, run.ID)
	if err != nil {
		return err
	}
	lastSuccess := sql.NullString{}
	if run.Status == "ok" {
		lastSuccess = sql.NullString{String: finished, Valid: true}
	}
	if run.Status == "ok" {
		_, err = tx.ExecContext(ctx, `UPDATE root_states SET last_crawl_status = ?, last_successful_crawl_at = ?, last_error = '' WHERE root_id = ?`,
			run.Status, lastSuccess.String, run.RootID)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE root_states SET last_crawl_status = ?, last_error = ? WHERE root_id = ?`,
			run.Status, run.ErrorMessage, run.RootID)
	}
	if err != nil {
		return err
	}
	if run.Status == "ok" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM crawl_checkpoints WHERE root_id = ?`, run.RootID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (c *Catalog) MarkOpenCrawlsInterrupted(ctx context.Context) (int64, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	finished := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE crawl_runs
		SET finished_at = ?, status = 'interrupted', error_message = 'service stopped before this crawl finished'
		WHERE finished_at IS NULL AND status IN ('running', 'hint_running')`, finished)
	if err != nil {
		return 0, err
	}
	changed, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, `UPDATE root_states
		SET last_crawl_status = 'interrupted', last_error = 'service stopped before the previous crawl finished'
		WHERE last_crawl_status IN ('running', 'hint_running')`); err != nil {
		return 0, err
	}
	return changed, tx.Commit()
}

func (c *Catalog) UpsertDocument(ctx context.Context, doc Document) (UpsertResult, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	var deleted int
	if err := c.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deleted_roots WHERE root_id = ?)`, doc.RootID).Scan(&deleted); err != nil {
		return UpsertResult{}, err
	}
	if deleted != 0 {
		return UpsertResult{Unchanged: true}, nil
	}
	if doc.AccessStatus == "" {
		doc.AccessStatus = "metadata_readable"
	}
	var existingID, existingSignature string
	err := c.db.QueryRowContext(ctx, `SELECT id, signature FROM documents WHERE root_id = ? AND normalized_path = ?`, doc.RootID, doc.NormalizedPath).Scan(&existingID, &existingSignature)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return UpsertResult{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	mod := doc.ModifiedAt.UTC().Format(time.RFC3339Nano)
	created := ""
	if !doc.CreatedAt.IsZero() {
		created = doc.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if errors.Is(err, sql.ErrNoRows) {
		movedFromPath := ""
		var movedID string
		if moveErr := c.db.QueryRowContext(ctx, `SELECT id, path FROM documents WHERE root_id = ? AND signature = ? AND status = 'missing' ORDER BY last_indexed_at DESC LIMIT 1`, doc.RootID, doc.Signature).Scan(&movedID, &movedFromPath); moveErr == nil {
			doc.MovedFromPath = movedFromPath
		}
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return UpsertResult{}, err
		}
		defer tx.Rollback()
		if movedID != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE documents SET status = 'moved' WHERE id = ?`, movedID); err != nil {
				return UpsertResult{}, err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO documents(id, root_id, path, normalized_path, name, extension, size, modified_at, created_at, status, last_seen_generation, last_indexed_at, content_status, ocr_status, content_text, content_hash, hash_status, owner, access_status, moved_from_path, is_folder, signature, missing_count)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, 'not_indexed', 'not_requested', '', '', 'not_hashed', ?, ?, ?, ?, ?, 0)`,
			doc.ID, doc.RootID, doc.Path, doc.NormalizedPath, doc.Name, doc.Extension, doc.Size, mod, created, doc.LastSeenGeneration, now, doc.Owner, doc.AccessStatus, doc.MovedFromPath, doc.IsFolder, doc.Signature)
		if err != nil {
			return UpsertResult{}, err
		}
		if err := upsertFTS(ctx, tx, doc.ID, doc.RootID, doc.Name, doc.Path, doc.Extension, ""); err != nil {
			return UpsertResult{}, err
		}
		return UpsertResult{Added: true}, tx.Commit()
	}
	if existingSignature == doc.Signature {
		_, err := c.db.ExecContext(ctx, `UPDATE documents SET status = 'active', access_status = ?, last_seen_generation = ?, missing_count = 0 WHERE id = ?`,
			doc.AccessStatus,
			doc.LastSeenGeneration, existingID)
		return UpsertResult{Unchanged: true}, err
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return UpsertResult{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE documents
		SET path = ?, name = ?, extension = ?, size = ?, modified_at = ?, created_at = ?, status = 'active',
		    last_seen_generation = ?, last_indexed_at = ?, content_status = 'not_indexed', ocr_status = 'not_requested', content_text = '', content_hash = '', hash_status = 'not_hashed', owner = ?, access_status = ?, is_folder = ?, signature = ?, missing_count = 0
		WHERE id = ?`,
		doc.Path, doc.Name, doc.Extension, doc.Size, mod, created, doc.LastSeenGeneration, now, doc.Owner, doc.AccessStatus, doc.IsFolder, doc.Signature, existingID)
	if err != nil {
		return UpsertResult{}, err
	}
	if err := upsertFTS(ctx, tx, existingID, doc.RootID, doc.Name, doc.Path, doc.Extension, ""); err != nil {
		return UpsertResult{}, err
	}
	return UpsertResult{Updated: true}, tx.Commit()
}

// UpsertDocuments commits a metadata batch in one SQLite transaction. Full
// crawls use this path to avoid serializing one durable commit per file.
func (c *Catalog) UpsertDocuments(ctx context.Context, docs []Document) ([]UpsertResult, error) {
	if len(docs) == 0 {
		return nil, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	results := make([]UpsertResult, 0, len(docs))
	for _, doc := range docs {
		result, err := upsertDocumentTx(ctx, tx, doc)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

func upsertDocumentTx(ctx context.Context, tx *sql.Tx, doc Document) (UpsertResult, error) {
	var deleted int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM deleted_roots WHERE root_id = ?)`, doc.RootID).Scan(&deleted); err != nil {
		return UpsertResult{}, err
	}
	if deleted != 0 {
		return UpsertResult{Unchanged: true}, nil
	}
	if doc.AccessStatus == "" {
		doc.AccessStatus = "metadata_readable"
	}
	var existingID, existingSignature string
	err := tx.QueryRowContext(ctx, `SELECT id, signature FROM documents WHERE root_id = ? AND normalized_path = ?`, doc.RootID, doc.NormalizedPath).Scan(&existingID, &existingSignature)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return UpsertResult{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	modified := doc.ModifiedAt.UTC().Format(time.RFC3339Nano)
	created := ""
	if !doc.CreatedAt.IsZero() {
		created = doc.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if errors.Is(err, sql.ErrNoRows) {
		movedFromPath := ""
		var movedID string
		if moveErr := tx.QueryRowContext(ctx, `SELECT id, path FROM documents WHERE root_id = ? AND signature = ? AND status = 'missing' ORDER BY last_indexed_at DESC LIMIT 1`, doc.RootID, doc.Signature).Scan(&movedID, &movedFromPath); moveErr == nil {
			doc.MovedFromPath = movedFromPath
		}
		if movedID != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE documents SET status = 'moved' WHERE id = ?`, movedID); err != nil {
				return UpsertResult{}, err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO documents(id, root_id, path, normalized_path, name, extension, size, modified_at, created_at, status, last_seen_generation, last_indexed_at, content_status, ocr_status, content_text, content_hash, hash_status, owner, access_status, moved_from_path, is_folder, signature, missing_count)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, 'not_indexed', 'not_requested', '', '', 'not_hashed', ?, ?, ?, ?, ?, 0)`,
			doc.ID, doc.RootID, doc.Path, doc.NormalizedPath, doc.Name, doc.Extension, doc.Size, modified, created, doc.LastSeenGeneration, now, doc.Owner, doc.AccessStatus, doc.MovedFromPath, doc.IsFolder, doc.Signature); err != nil {
			return UpsertResult{}, err
		}
		if err := upsertFTS(ctx, tx, doc.ID, doc.RootID, doc.Name, doc.Path, doc.Extension, ""); err != nil {
			return UpsertResult{}, err
		}
		return UpsertResult{Added: true}, nil
	}
	if existingSignature == doc.Signature {
		_, err := tx.ExecContext(ctx, `UPDATE documents SET status = 'active', access_status = ?, last_seen_generation = ?, missing_count = 0 WHERE id = ?`, doc.AccessStatus, doc.LastSeenGeneration, existingID)
		return UpsertResult{Unchanged: true}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE documents
		SET path = ?, name = ?, extension = ?, size = ?, modified_at = ?, created_at = ?, status = 'active',
		    last_seen_generation = ?, last_indexed_at = ?, content_status = 'not_indexed', ocr_status = 'not_requested', content_text = '', content_hash = '', hash_status = 'not_hashed', owner = ?, access_status = ?, is_folder = ?, signature = ?, missing_count = 0
		WHERE id = ?`,
		doc.Path, doc.Name, doc.Extension, doc.Size, modified, created, doc.LastSeenGeneration, now, doc.Owner, doc.AccessStatus, doc.IsFolder, doc.Signature, existingID); err != nil {
		return UpsertResult{}, err
	}
	if err := upsertFTS(ctx, tx, existingID, doc.RootID, doc.Name, doc.Path, doc.Extension, ""); err != nil {
		return UpsertResult{}, err
	}
	return UpsertResult{Updated: true}, nil
}

func upsertFTS(ctx context.Context, tx *sql.Tx, id, rootID, name, path, extension, content string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM documents_fts WHERE id = ?`, id); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO documents_fts(id, root_id, name, path, extension, content) VALUES (?, ?, ?, ?, ?, ?)`, id, rootID, name, path, extension, content)
	return err
}

func (c *Catalog) UpdateExtractedContent(ctx context.Context, id, signature, status, content string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rootID, name, path, extension string
	if err := tx.QueryRowContext(ctx, `SELECT root_id, name, path, extension FROM documents WHERE id = ? AND signature = ?`, id, signature).Scan(&rootID, &name, &path, &extension); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE documents SET content_status = ?, content_text = ? WHERE id = ?`, status, content, id); err != nil {
		return err
	}
	if err := upsertFTS(ctx, tx, id, rootID, name, path, extension, content); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Catalog) UpdateOCRStatus(ctx context.Context, id, signature, status string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.db.ExecContext(ctx, `UPDATE documents SET ocr_status = ? WHERE id = ? AND signature = ?`, status, id, signature)
	return err
}

func (c *Catalog) UpdateContentStatus(ctx context.Context, id, signature, status string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.db.ExecContext(ctx, `UPDATE documents SET content_status = ? WHERE id = ? AND signature = ?`, status, id, signature)
	return err
}

func (c *Catalog) UpdateOCRContent(ctx context.Context, id, signature, status, content string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var rootID, name, path, extension string
	if err := tx.QueryRowContext(ctx, `SELECT root_id, name, path, extension FROM documents WHERE id = ? AND signature = ?`, id, signature).Scan(&rootID, &name, &path, &extension); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE documents SET content_status = 'extracted', content_text = ?, ocr_status = ? WHERE id = ?`, content, status, id); err != nil {
		return err
	}
	if err := upsertFTS(ctx, tx, id, rootID, name, path, extension, content); err != nil {
		return err
	}
	return tx.Commit()
}

func (c *Catalog) UpdateContentHash(ctx context.Context, id, signature, hash, status string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.db.ExecContext(ctx, `UPDATE documents SET content_hash = ?, hash_status = ? WHERE id = ? AND signature = ?`, hash, status, id, signature)
	return err
}

func (c *Catalog) ReleaseContentClaim(ctx context.Context, id, signature string) error {
	return c.releaseBackgroundClaim(ctx, "content_status", "not_indexed", id, signature)
}

func (c *Catalog) ReleaseOCRClaim(ctx context.Context, id, signature string) error {
	return c.releaseBackgroundClaim(ctx, "ocr_status", "pending", id, signature)
}

func (c *Catalog) ReleaseHashClaim(ctx context.Context, id, signature string) error {
	return c.releaseBackgroundClaim(ctx, "hash_status", "not_hashed", id, signature)
}

func (c *Catalog) releaseBackgroundClaim(ctx context.Context, column, pending, id, signature string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.db.ExecContext(ctx, `UPDATE documents SET `+column+` = ? WHERE id = ? AND signature = ? AND `+column+` = 'queued'`, pending, id, signature)
	return err
}

// ResetQueuedBackground releases work that was claimed before a controlled
// stop. No source or document metadata is changed; a subsequent refill simply
// claims it again using the current path and signature.
func (c *Catalog) ResetQueuedBackground(ctx context.Context) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, update := range []string{
		`UPDATE documents SET content_status = 'not_indexed' WHERE content_status = 'queued'`,
		`UPDATE documents SET ocr_status = 'pending' WHERE ocr_status = 'queued'`,
		`UPDATE documents SET hash_status = 'not_hashed' WHERE hash_status = 'queued'`,
	} {
		if _, err := tx.ExecContext(ctx, update); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (c *Catalog) ClaimPendingContent(ctx context.Context, rootIDs []string, limit int, maxSize int64) ([]BackgroundCandidate, error) {
	return c.claimPending(ctx, "content_status", "not_indexed", "queued", rootIDs, extract.IndexableExtensions(), limit, maxSize)
}

func (c *Catalog) ClaimPendingOCR(ctx context.Context, rootIDs []string, limit int, maxSize int64) ([]BackgroundCandidate, error) {
	return c.claimPending(ctx, "ocr_status", "pending", "queued", rootIDs, nil, limit, maxSize)
}

func (c *Catalog) ClaimPendingHashes(ctx context.Context, rootIDs []string, limit int, maxSize int64) ([]BackgroundCandidate, error) {
	return c.claimPending(ctx, "hash_status", "not_hashed", "queued", rootIDs, nil, limit, maxSize)
}

func (c *Catalog) claimPending(ctx context.Context, column, pending, claimed string, rootIDs, extensions []string, limit int, maxSize int64) ([]BackgroundCandidate, error) {
	if len(rootIDs) == 0 || limit <= 0 || maxSize <= 0 {
		return nil, nil
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rootPlaceholders := strings.TrimRight(strings.Repeat("?,", len(rootIDs)), ",")
	args := []any{pending, maxSize}
	for _, rootID := range rootIDs {
		args = append(args, rootID)
	}
	extensionClause := ""
	if len(extensions) > 0 {
		extensionClause = " AND extension IN (" + placeholders(len(extensions)) + ")"
		for _, extension := range extensions {
			args = append(args, extension)
		}
	}
	args = append(args, limit)
	// New or changed documents receive a fresh last_indexed_at value. Prefer
	// them over the historical backlog so watcher hints reach the secondary
	// content/OCR/hash pass promptly after structural indexing is idle.
	query := `SELECT id, root_id, path, signature, size FROM documents WHERE status = 'active' AND is_folder = 0 AND ` + column + ` = ? AND size <= ? AND root_id IN (` + rootPlaceholders + `)` + extensionClause + ` ORDER BY last_indexed_at DESC, path ASC LIMIT ?`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var candidates []BackgroundCandidate
	for rows.Next() {
		var item BackgroundCandidate
		if err := rows.Scan(&item.ID, &item.RootID, &item.Path, &item.Signature, &item.Size); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, item := range candidates {
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET `+column+` = ? WHERE id = ? AND `+column+` = ?`, claimed, item.ID, pending); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return candidates, nil
}

func (c *Catalog) MarkMissing(ctx context.Context, rootID string, generation int64, deleteAfter int) (int64, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE documents
		SET status = 'missing', access_status = 'missing', missing_count = missing_count + 1
		WHERE root_id = ? AND status = 'active' AND last_seen_generation <> ?`,
		rootID, generation)
	if err != nil {
		return 0, err
	}
	missing, _ := res.RowsAffected()
	if deleteAfter > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET status = 'deleted' WHERE root_id = ? AND status = 'missing' AND missing_count >= ?`, rootID, deleteAfter); err != nil {
			return 0, err
		}
	}
	return missing, tx.Commit()
}

// MissingDocumentPaths returns records from the previous pass that need a
// first-class recheck before ordinary traversal resumes. The limit keeps a
// temporarily unavailable share from turning recovery into an unbounded queue.
func (c *Catalog) MissingDocumentPaths(ctx context.Context, rootID string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := c.db.QueryContext(ctx, `SELECT path FROM documents
		WHERE root_id = ? AND status = 'missing'
		ORDER BY missing_count DESC, last_indexed_at ASC, path ASC LIMIT ?`, rootID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paths := make([]string, 0, limit)
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

func (c *Catalog) MarkPathMissing(ctx context.Context, rootID string, path string) (int64, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	normalized := NormalizePath(path)
	res, err := c.db.ExecContext(ctx, `UPDATE documents
		SET status = 'missing', access_status = 'missing', missing_count = missing_count + 1
		WHERE root_id = ? AND status = 'active' AND (normalized_path = ? OR normalized_path LIKE ?)`,
		rootID, normalized, descendantPathLike(normalized))
	if err != nil {
		return 0, err
	}
	count, _ := res.RowsAffected()
	return count, nil
}

// ReconcileMoves converts an unambiguous missing-to-active hash match into a
// move relationship after a full crawl. Duplicate hashes are intentionally left alone.
func (c *Catalog) ReconcileMoves(ctx context.Context, rootID string, generation int64) (int64, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT active.id, missing.id, missing.path
		FROM documents active JOIN documents missing
		ON missing.root_id = active.root_id AND missing.content_hash = active.content_hash
		WHERE active.root_id = ? AND active.status = 'active' AND active.last_seen_generation = ?
		AND active.content_hash <> '' AND active.moved_from_path = '' AND missing.status = 'missing'`, rootID, generation)
	if err != nil {
		return 0, err
	}
	type candidate struct{ activeID, missingID, missingPath string }
	byActive := map[string][]candidate{}
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.activeID, &item.missingID, &item.missingPath); err != nil {
			rows.Close()
			return 0, err
		}
		byActive[item.activeID] = append(byActive[item.activeID], item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var moved int64
	for _, matches := range byActive {
		if len(matches) != 1 {
			continue
		}
		match := matches[0]
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET status = 'moved' WHERE id = ?`, match.missingID); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE documents SET moved_from_path = ? WHERE id = ?`, match.missingPath, match.activeID); err != nil {
			return 0, err
		}
		moved++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return moved, nil
}

func (c *Catalog) MarkDirectoryCheckpoint(ctx context.Context, rootID string, path string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.db.ExecContext(ctx, `INSERT INTO crawl_checkpoints(root_id, normalized_path, path, completed_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(root_id, normalized_path) DO UPDATE SET path = excluded.path, completed_at = excluded.completed_at`,
		rootID, NormalizePath(path), path, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// ClearRootCheckpoints forces a complete re-enumeration without changing any
// document state. It is used after an incomplete traversal or path migration,
// where resuming a completed parent could otherwise hide unfinished children.
func (c *Catalog) ClearRootCheckpoints(ctx context.Context, rootID string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.db.ExecContext(ctx, `DELETE FROM crawl_checkpoints WHERE root_id = ?`, rootID)
	return err
}

func (c *Catalog) DirectoryCheckpointed(ctx context.Context, rootID string, path string) (bool, error) {
	var existing string
	err := c.db.QueryRowContext(ctx, `SELECT normalized_path FROM crawl_checkpoints WHERE root_id = ? AND normalized_path = ?`,
		rootID, NormalizePath(path)).Scan(&existing)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (c *Catalog) TouchPathGeneration(ctx context.Context, rootID string, path string, generation int64) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	normalized := NormalizePath(path)
	_, err := c.db.ExecContext(ctx, `UPDATE documents
		SET last_seen_generation = ?
		WHERE root_id = ? AND status = 'active' AND (normalized_path = ? OR normalized_path LIKE ?)`,
		generation, rootID, normalized, descendantPathLike(normalized))
	return err
}

func descendantPathLike(normalized string) string {
	separator := string(filepath.Separator)
	if strings.HasSuffix(normalized, separator) {
		return normalized + "%"
	}
	return normalized + separator + "%"
}

func (c *Catalog) Search(ctx context.Context, req SearchRequest, maxResults int) (SearchResponse, error) {
	start := time.Now()
	if req.Offset < 0 {
		return SearchResponse{}, &RequestError{Code: "invalid_offset", Message: "offset must be zero or greater"}
	}
	limit := req.Limit
	if limit <= 0 || limit > maxResults {
		limit = maxResults
	}
	if limit <= 0 {
		limit = 50
	}
	if len(req.Filters.Roots) > 100 {
		return SearchResponse{}, &RequestError{Code: "too_many_roots", Message: "at most 100 roots are supported"}
	}
	if len(req.Filters.Extensions) > 100 {
		return SearchResponse{}, &RequestError{Code: "too_many_extensions", Message: "at most 100 extensions are supported"}
	}
	if req.Filters.ModifiedAfter != "" {
		value, err := time.Parse(time.RFC3339, req.Filters.ModifiedAfter)
		if err != nil {
			return SearchResponse{}, &RequestError{Code: "invalid_modified_after", Message: "modified_after must be RFC3339"}
		}
		req.Filters.ModifiedAfter = value.UTC().Format(time.RFC3339Nano)
	}
	if req.Filters.ModifiedBefore != "" {
		value, err := time.Parse(time.RFC3339, req.Filters.ModifiedBefore)
		if err != nil {
			return SearchResponse{}, &RequestError{Code: "invalid_modified_before", Message: "modified_before must be RFC3339"}
		}
		req.Filters.ModifiedBefore = value.UTC().Format(time.RFC3339Nano)
	}
	args := []any{}
	where := []string{"d.status = 'active'"}
	from := "documents d"
	if req.Filters.Boolean.TermCount() > 100 {
		return SearchResponse{}, &RequestError{Code: "too_many_boolean_terms", Message: "at most 100 boolean terms are supported"}
	}
	hasQuery := strings.TrimSpace(req.Query) != "" || req.Filters.Boolean.TermCount() > 0
	highlightQuery := strings.Join(append(append([]string{req.Query}, req.Filters.Boolean.All...), req.Filters.Boolean.Any...), " ")
	if hasQuery {
		matchQuery, err := scopedFTSQuery(req.Query, req.Filters.MatchFields, req.Filters.MatchMode, req.Filters.Boolean)
		if err != nil {
			return SearchResponse{}, err
		}
		from = "documents d JOIN documents_fts ON documents_fts.id = d.id"
		// Files can match any requested indexed field, including their parent
		// path. Folders represent destinations, so an ancestor-path match must
		// not make an unrelated descendant folder a result.
		where = append(where, "documents_fts MATCH ?")
		args = append(args, matchQuery)
		folderClause, folderArgs, err := folderNameMatchClause(req.Query, req.Filters.MatchMode, req.Filters.Boolean)
		if err != nil {
			return SearchResponse{}, err
		}
		where = append(where, "(d.is_folder = 0 OR ("+folderClause+"))")
		args = append(args, folderArgs...)
	}
	if len(req.Filters.Roots) > 0 {
		where = append(where, "d.root_id IN ("+placeholders(len(req.Filters.Roots))+")")
		for _, v := range req.Filters.Roots {
			args = append(args, v)
		}
	}
	if len(req.Filters.Extensions) > 0 {
		where = append(where, "d.extension IN ("+placeholders(len(req.Filters.Extensions))+")")
		for _, v := range req.Filters.Extensions {
			args = append(args, strings.TrimPrefix(strings.ToLower(v), "."))
		}
	}
	prefixes := append([]string(nil), req.Filters.PathPrefixes...)
	prefixes = append(prefixes, req.Filters.IncludePaths...)
	if strings.TrimSpace(req.Filters.PathPrefix) != "" {
		prefixes = append(prefixes, req.Filters.PathPrefix)
	}
	if len(prefixes)+len(req.Filters.ExcludePaths) > 100 {
		return SearchResponse{}, &RequestError{Code: "too_many_path_scopes", Message: "at most 100 include and exclude paths are supported"}
	}
	if len(prefixes) > 0 {
		clauses := make([]string, 0, len(prefixes))
		for _, prefix := range prefixes {
			prefix = strings.TrimSpace(prefix)
			if prefix == "" {
				continue
			}
			clauses = append(clauses, "(d.normalized_path = ? OR d.normalized_path LIKE ? ESCAPE '\\')")
			args = append(args, NormalizePath(prefix), escapeLike(pathChildPrefix(prefix))+"%")
		}
		if len(clauses) > 0 {
			where = append(where, "("+strings.Join(clauses, " OR ")+")")
		}
	}
	if len(req.Filters.ExcludePaths) > 0 {
		for _, prefix := range req.Filters.ExcludePaths {
			prefix = strings.TrimSpace(prefix)
			if prefix == "" {
				continue
			}
			clause := "NOT (d.normalized_path = ? OR d.normalized_path LIKE ? ESCAPE '\\')"
			args = append(args, NormalizePath(prefix), escapeLike(pathChildPrefix(prefix))+"%")
			overrides := []string{}
			for _, include := range prefixes {
				if pathScopeWithin(include, prefix) {
					overrides = append(overrides, "(d.normalized_path = ? OR d.normalized_path LIKE ? ESCAPE '\\')")
					args = append(args, NormalizePath(include), escapeLike(pathChildPrefix(include))+"%")
				}
			}
			if len(overrides) > 0 {
				clause = "(" + clause + " OR " + strings.Join(overrides, " OR ") + ")"
			}
			where = append(where, clause)
		}
	}
	if req.Filters.Kind != "" {
		switch strings.ToLower(req.Filters.Kind) {
		case "file":
			where = append(where, "d.is_folder = 0")
		case "folder", "directory":
			where = append(where, "d.is_folder = 1")
		default:
			return SearchResponse{}, &RequestError{Code: "invalid_kind", Message: "kind must be file or folder"}
		}
	}
	if req.Filters.ModifiedAfter != "" {
		where = append(where, "d.modified_at >= ?")
		args = append(args, req.Filters.ModifiedAfter)
	}
	if req.Filters.ModifiedBefore != "" {
		where = append(where, "d.modified_at <= ?")
		args = append(args, req.Filters.ModifiedBefore)
	}
	if req.Filters.MinSize != nil {
		where = append(where, "d.size >= ?")
		args = append(args, *req.Filters.MinSize)
	}
	if req.Filters.MaxSize != nil {
		where = append(where, "d.size <= ?")
		args = append(args, *req.Filters.MaxSize)
	}
	direction := req.SortDirection
	if direction == "" {
		direction = req.Direction
	}
	order, err := searchOrder(req.Sort, direction, hasQuery)
	if err != nil {
		return SearchResponse{}, err
	}
	query := `SELECT d.id, d.root_id, d.path, d.normalized_path, d.name, d.extension, d.is_folder, d.size, d.modified_at, d.created_at, d.status, d.last_seen_generation, d.last_indexed_at, d.content_status, d.ocr_status, d.content_text, d.content_hash, d.hash_status, d.owner, d.access_status, d.moved_from_path, d.signature
		FROM ` + from + ` WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, limit+1, req.Offset)
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return SearchResponse{}, err
	}
	defer rows.Close()
	docs := make([]Document, 0)
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return SearchResponse{}, err
		}
		doc.ResultID = doc.ID
		doc.ETag = doc.Signature
		if doc.IsFolder {
			doc.Kind = "folder"
		} else {
			doc.Kind = "file"
		}
		if hasQuery {
			addMatchMetadata(&doc, highlightQuery, req.Filters.MatchFields)
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return SearchResponse{}, err
	}
	resp := SearchResponse{SearchID: req.SearchID, Results: docs, Offset: req.Offset, TookMS: time.Since(start).Milliseconds()}
	if len(docs) > limit {
		resp.HasMore = true
		resp.Results = docs[:limit]
		next := req.Offset + limit
		resp.NextOffset = &next
	}
	states, err := c.RootStates(ctx)
	if err == nil {
		resp.Index = searchIndexInfo(states, req.Filters.Roots)
	}
	return resp, nil
}

func pathScopeWithin(path, parent string) bool {
	path, parent = NormalizePath(path), NormalizePath(parent)
	if path == parent {
		return true
	}
	return strings.HasPrefix(path, pathChildPrefix(parent))
}

func searchIndexInfo(states map[string]RootState, requestedRoots []string) SearchIndexInfo {
	selected := map[string]bool{}
	for _, rootID := range requestedRoots {
		selected[rootID] = true
	}
	now := time.Now()
	info := SearchIndexInfo{Roots: []SearchRootStatus{}}
	for rootID, state := range states {
		if len(selected) > 0 && !selected[rootID] {
			continue
		}
		status := SearchRootStatus{RootID: rootID, Generation: state.LastGeneration, LastStatus: state.LastCrawlStatus, LastSuccessfulCrawlAt: state.LastSuccessfulCrawlAt}
		if state.LastGeneration > info.Generation {
			info.Generation = state.LastGeneration
		}
		if state.LastSuccessfulCrawlAt != nil {
			seconds := int64(now.Sub(*state.LastSuccessfulCrawlAt).Seconds())
			if seconds < 0 {
				seconds = 0
			}
			status.FreshnessSeconds = &seconds
			if info.FreshnessSeconds == nil || seconds > *info.FreshnessSeconds {
				info.FreshnessSeconds = &seconds
			}
		}
		info.Roots = append(info.Roots, status)
	}
	return info
}

func searchOrder(sort, direction string, hasQuery bool) (string, error) {
	sort = strings.ToLower(strings.TrimSpace(sort))
	if sort == "" {
		if hasQuery {
			sort = "relevance"
		} else {
			sort = "modified"
		}
	}
	direction = strings.ToLower(strings.TrimSpace(direction))
	if direction == "" {
		if sort == "name" {
			direction = "asc"
		} else {
			direction = "desc"
		}
	}
	if direction != "asc" && direction != "desc" {
		return "", &RequestError{Code: "invalid_sort_direction", Message: "direction must be asc or desc"}
	}
	dir := strings.ToUpper(direction)
	switch sort {
	case "name":
		return "d.name COLLATE NOCASE " + dir + ", d.normalized_path ASC", nil
	case "modified":
		return "d.modified_at " + dir + ", d.normalized_path ASC", nil
	case "size":
		return "d.size " + dir + ", d.normalized_path ASC", nil
	case "relevance":
		if !hasQuery {
			return "", &RequestError{Code: "invalid_sort", Message: "relevance sort requires a query"}
		}
		// SQLite bm25 returns lower scores for better matches. Keep the public
		// direction conventional: descending relevance means best match first.
		rankDir := "ASC"
		if dir == "ASC" {
			rankDir = "DESC"
		}
		return "bm25(documents_fts) " + rankDir + ", d.normalized_path ASC", nil
	default:
		return "", &RequestError{Code: "invalid_sort", Message: "sort must be name, modified, size, or relevance"}
	}
}

func pathChildPrefix(path string) string {
	normalized := NormalizePath(path)
	separator := string(filepath.Separator)
	if isWindowsRootPath(normalized) {
		separator = "\\"
	}
	if strings.HasSuffix(normalized, separator) {
		return normalized
	}
	return normalized + separator
}

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "%", "\\%")
	return strings.ReplaceAll(value, "_", "\\_")
}

func (c *Catalog) RootStates(ctx context.Context) (map[string]RootState, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT root_id, last_generation, last_successful_crawl_at, last_crawl_status, last_error FROM root_states`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RootState{}
	for rows.Next() {
		var s RootState
		var success sql.NullString
		if err := rows.Scan(&s.RootID, &s.LastGeneration, &success, &s.LastCrawlStatus, &s.LastError); err != nil {
			return nil, err
		}
		if success.Valid && success.String != "" {
			t, _ := time.Parse(time.RFC3339Nano, success.String)
			s.LastSuccessfulCrawlAt = &t
		}
		out[s.RootID] = s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id, state := range out {
		_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents WHERE root_id = ? AND status = 'active'`, id).Scan(&state.DocumentCount)
		_ = c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents WHERE root_id = ? AND status = 'missing'`, id).Scan(&state.MissingCount)
		out[id] = state
	}
	return out, nil
}

func (c *Catalog) RecentCrawls(ctx context.Context, limit int) ([]CrawlRun, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := c.db.QueryContext(ctx, `SELECT id, root_id, generation, started_at, finished_at, status, files_seen, files_added, files_updated, files_unchanged, files_missing, errors, error_message
		FROM crawl_runs ORDER BY started_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []CrawlRun
	for rows.Next() {
		var run CrawlRun
		var started string
		var finished sql.NullString
		if err := rows.Scan(&run.ID, &run.RootID, &run.Generation, &started, &finished, &run.Status, &run.FilesSeen, &run.FilesAdded, &run.FilesUpdated, &run.FilesUnchanged, &run.FilesMissing, &run.Errors, &run.ErrorMessage); err != nil {
			return nil, err
		}
		run.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		if finished.Valid && finished.String != "" {
			t, _ := time.Parse(time.RFC3339Nano, finished.String)
			run.FinishedAt = &t
		}
		runs = append(runs, run)
	}
	return runs, rows.Err()
}

type documentScanner interface {
	Scan(dest ...any) error
}

func scanDocument(row documentScanner) (Document, error) {
	var d Document
	var modified, created, indexed string
	if err := row.Scan(&d.ID, &d.RootID, &d.Path, &d.NormalizedPath, &d.Name, &d.Extension, &d.IsFolder, &d.Size, &modified, &created, &d.Status, &d.LastSeenGeneration, &indexed, &d.ContentStatus, &d.OCRStatus, &d.ContentText, &d.ContentHash, &d.HashStatus, &d.Owner, &d.AccessStatus, &d.MovedFromPath, &d.Signature); err != nil {
		return d, err
	}
	d.ModifiedAt, _ = time.Parse(time.RFC3339Nano, modified)
	if created != "" {
		d.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	}
	d.LastIndexedAt, _ = time.Parse(time.RFC3339Nano, indexed)
	d.ResultID = d.ID
	d.ETag = d.Signature
	if d.IsFolder {
		d.Kind = "folder"
	} else {
		d.Kind = "file"
	}
	return d, nil
}

func addMatchMetadata(doc *Document, query string, requestedFields []string) {
	terms := strings.Fields(strings.ToLower(query))
	if len(terms) == 0 {
		return
	}
	selected := map[string]bool{}
	for _, field := range requestedFields {
		selected[strings.ToLower(strings.TrimSpace(field))] = true
	}
	fields := []struct{ name, value string }{{"name", doc.Name}, {"path", doc.Path}, {"extension", doc.Extension}, {"content", doc.ContentText}}
	doc.Highlights = map[string][]string{}
	for _, field := range fields {
		if len(selected) > 0 && !selected[field.name] {
			continue
		}
		if excerpt, ok := matchedExcerpt(field.value, terms); ok {
			doc.MatchedFields = append(doc.MatchedFields, field.name)
			doc.Highlights[field.name] = []string{excerpt}
		}
	}
	if len(doc.MatchedFields) == 0 {
		doc.MatchedFields = []string{"metadata"}
	}
	if len(doc.Highlights) == 0 {
		doc.Highlights = nil
	}
}

func matchedExcerpt(value string, terms []string) (string, bool) {
	lower := strings.ToLower(value)
	position := -1
	for _, term := range terms {
		if i := strings.Index(lower, term); i >= 0 && (position < 0 || i < position) {
			position = i
		}
	}
	if position < 0 {
		return "", false
	}
	start, end := position-96, position+192
	if start < 0 {
		start = 0
	}
	if end > len(value) {
		end = len(value)
	}
	excerpt := value[start:end]
	if start > 0 {
		excerpt = "..." + excerpt
	}
	if end < len(value) {
		excerpt += "..."
	}
	return excerpt, true
}

func placeholders(n int) string {
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

func escapeFTS(q string) string {
	parts := strings.Fields(q)
	for i, part := range parts {
		// Quoted prefix terms preserve punctuation safety while letting a user
		// type the beginning of a filename or path component naturally.
		parts[i] = `"` + strings.ReplaceAll(part, `"`, `""`) + `"*`
	}
	return strings.Join(parts, " ")
}

func scopedFTSQuery(query string, fields []string, matchMode string, boolean BooleanFilter) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(matchMode))
	if mode == "" {
		mode = "prefix"
	}
	if mode != "prefix" && mode != "exact" {
		return "", &RequestError{Code: "invalid_match_mode", Message: "match_mode supports prefix and exact"}
	}
	terms, err := escapedQueryTerms(query, mode, boolean)
	if err != nil {
		return "", err
	}
	if len(fields) == 0 {
		return terms, nil
	}
	valid := map[string]bool{"name": true, "path": true, "extension": true, "content": true}
	selected := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, field := range fields {
		field = strings.ToLower(strings.TrimSpace(field))
		if !valid[field] {
			return "", &RequestError{Code: "invalid_match_fields", Message: "match_fields supports name, path, extension, and content"}
		}
		if !seen[field] {
			selected = append(selected, field)
			seen[field] = true
		}
	}
	if len(selected) == 0 {
		return "", &RequestError{Code: "invalid_match_fields", Message: "match_fields must not be empty when provided"}
	}
	return "{" + strings.Join(selected, " ") + "} : (" + terms + ")", nil
}

func escapedQueryTerms(query, mode string, boolean BooleanFilter) (string, error) {
	term := func(value string) (string, error) {
		value = strings.TrimSpace(value)
		if value == "" {
			return "", &RequestError{Code: "invalid_boolean_filter", Message: "boolean terms must not be empty"}
		}
		if mode == "prefix" {
			return escapeFTS(value), nil
		}
		return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`, nil
	}
	positive := []string{}
	if strings.TrimSpace(query) != "" {
		value, err := term(query)
		if err != nil {
			return "", err
		}
		positive = append(positive, "("+value+")")
	}
	for _, raw := range boolean.All {
		value, err := term(raw)
		if err != nil {
			return "", err
		}
		positive = append(positive, "("+value+")")
	}
	if len(boolean.Any) > 0 {
		any := make([]string, 0, len(boolean.Any))
		for _, raw := range boolean.Any {
			value, err := term(raw)
			if err != nil {
				return "", err
			}
			any = append(any, "("+value+")")
		}
		positive = append(positive, "("+strings.Join(any, " OR ")+")")
	}
	if len(positive) == 0 {
		return "", &RequestError{Code: "invalid_boolean_filter", Message: "boolean not terms require a query, all term, or any term"}
	}
	result := strings.Join(positive, " AND ")
	for _, raw := range boolean.Not {
		value, err := term(raw)
		if err != nil {
			return "", err
		}
		result = "(" + result + ") NOT (" + value + ")"
	}
	return result, nil
}

func folderNameMatchClause(query string, matchMode string, boolean BooleanFilter) (string, []any, error) {
	predicate := func(value string) (string, []any, error) {
		value = strings.TrimSpace(value)
		if value == "" {
			return "", nil, &RequestError{Code: "invalid_boolean_filter", Message: "boolean terms must not be empty"}
		}
		if strings.EqualFold(matchMode, "exact") {
			return "d.name = ? COLLATE NOCASE", []any{value}, nil
		}
		terms := strings.Fields(value)
		clauses, args := make([]string, 0, len(terms)), make([]any, 0, len(terms))
		for _, term := range terms {
			clauses, args = append(clauses, "d.name LIKE ? ESCAPE '\\' COLLATE NOCASE"), append(args, "%"+escapeLike(term)+"%")
		}
		return "(" + strings.Join(clauses, " AND ") + ")", args, nil
	}
	positive, args := []string{}, []any{}
	add := func(value string) error {
		clause, values, err := predicate(value)
		if err == nil {
			positive, args = append(positive, clause), append(args, values...)
		}
		return err
	}
	if strings.TrimSpace(query) != "" {
		if err := add(query); err != nil {
			return "", nil, err
		}
	}
	for _, value := range boolean.All {
		if err := add(value); err != nil {
			return "", nil, err
		}
	}
	if len(boolean.Any) > 0 {
		any, values := []string{}, []any{}
		for _, value := range boolean.Any {
			clause, termArgs, err := predicate(value)
			if err != nil {
				return "", nil, err
			}
			any, values = append(any, clause), append(values, termArgs...)
		}
		positive, args = append(positive, "("+strings.Join(any, " OR ")+")"), append(args, values...)
	}
	if len(positive) == 0 {
		return "", nil, &RequestError{Code: "invalid_boolean_filter", Message: "boolean not terms require a query, all term, or any term"}
	}
	result := strings.Join(positive, " AND ")
	for _, value := range boolean.Not {
		clause, termArgs, err := predicate(value)
		if err != nil {
			return "", nil, err
		}
		result, args = "("+result+") AND NOT ("+clause+")", append(args, termArgs...)
	}
	return result, args, nil
}

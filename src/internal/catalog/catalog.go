package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Catalog struct {
	db      *sql.DB
	writeMu sync.Mutex
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
	ContentHash        string              `json:"content_hash,omitempty"`
	HashStatus         string              `json:"hash_status,omitempty"`
	Owner              string              `json:"owner,omitempty"`
	AccessStatus       string              `json:"access_status"`
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
	Roots          []string `json:"roots"`
	Extensions     []string `json:"extensions"`
	PathPrefix     string   `json:"path_prefix"`
	PathPrefixes   []string `json:"path_prefixes"`
	IncludePaths   []string `json:"include_paths"`
	ExcludePaths   []string `json:"exclude_paths"`
	Kind           string   `json:"kind"`
	ModifiedAfter  string   `json:"modified_after"`
	ModifiedBefore string   `json:"modified_before"`
	MinSize        *int64   `json:"min_size"`
	MaxSize        *int64   `json:"max_size"`
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
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "qsurfer-search.db")
	db, err := sql.Open("sqlite", dbPath)
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
	return c.db.Close()
}

func (c *Catalog) migrate(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA synchronous=NORMAL;`,
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
			content_text TEXT NOT NULL DEFAULT '',
			content_hash TEXT NOT NULL DEFAULT '',
			hash_status TEXT NOT NULL DEFAULT 'not_hashed',
			owner TEXT NOT NULL DEFAULT '',
			access_status TEXT NOT NULL DEFAULT 'metadata_readable',
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
	if err := c.ensureColumn(ctx, "documents", "content_text", "TEXT NOT NULL DEFAULT ''"); err != nil {
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
	return strings.ToLower(filepath.Clean(path))
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

func (c *Catalog) UpsertDocument(ctx context.Context, doc Document) (UpsertResult, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
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
		tx, err := c.db.BeginTx(ctx, nil)
		if err != nil {
			return UpsertResult{}, err
		}
		defer tx.Rollback()
		_, err = tx.ExecContext(ctx, `INSERT INTO documents(id, root_id, path, normalized_path, name, extension, size, modified_at, created_at, status, last_seen_generation, last_indexed_at, content_status, content_text, content_hash, hash_status, owner, access_status, is_folder, signature, missing_count)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, 'not_indexed', '', '', 'not_hashed', ?, ?, ?, ?, 0)`,
			doc.ID, doc.RootID, doc.Path, doc.NormalizedPath, doc.Name, doc.Extension, doc.Size, mod, created, doc.LastSeenGeneration, now, doc.Owner, doc.AccessStatus, doc.IsFolder, doc.Signature)
		if err != nil {
			return UpsertResult{}, err
		}
		if err := upsertFTS(ctx, tx, doc.ID, doc.RootID, doc.Name, doc.Path, doc.Extension, ""); err != nil {
			return UpsertResult{}, err
		}
		return UpsertResult{Added: true}, tx.Commit()
	}
	if existingSignature == doc.Signature {
		_, err := c.db.ExecContext(ctx, `UPDATE documents SET status = 'active', last_seen_generation = ?, missing_count = 0 WHERE id = ?`,
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
		    last_seen_generation = ?, last_indexed_at = ?, content_status = 'not_indexed', content_text = '', content_hash = '', hash_status = 'not_hashed', owner = ?, access_status = ?, is_folder = ?, signature = ?, missing_count = 0
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

func (c *Catalog) UpdateContentHash(ctx context.Context, id, signature, hash, status string) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.db.ExecContext(ctx, `UPDATE documents SET content_hash = ?, hash_status = ? WHERE id = ? AND signature = ?`, hash, status, id, signature)
	return err
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
		SET status = 'missing', missing_count = missing_count + 1
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

func (c *Catalog) MarkPathMissing(ctx context.Context, rootID string, path string) (int64, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	normalized := NormalizePath(path)
	res, err := c.db.ExecContext(ctx, `UPDATE documents
		SET status = 'missing', missing_count = missing_count + 1
		WHERE root_id = ? AND status = 'active' AND (normalized_path = ? OR normalized_path LIKE ?)`,
		rootID, normalized, descendantPathLike(normalized))
	if err != nil {
		return 0, err
	}
	count, _ := res.RowsAffected()
	return count, nil
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
	args := []any{}
	where := []string{"d.status = 'active'"}
	from := "documents d"
	hasQuery := strings.TrimSpace(req.Query) != ""
	if hasQuery {
		from = "documents d JOIN documents_fts ON documents_fts.id = d.id"
		where = append(where, "documents_fts MATCH ?")
		args = append(args, escapeFTS(req.Query))
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
			where = append(where, "NOT (d.normalized_path = ? OR d.normalized_path LIKE ? ESCAPE '\\')")
			args = append(args, NormalizePath(prefix), escapeLike(pathChildPrefix(prefix))+"%")
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
	query := `SELECT d.id, d.root_id, d.path, d.normalized_path, d.name, d.extension, d.is_folder, d.size, d.modified_at, d.created_at, d.status, d.last_seen_generation, d.last_indexed_at, d.content_status, d.content_hash, d.hash_status, d.owner, d.access_status, d.signature
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
			doc.MatchedFields = []string{"name", "path"}
			doc.Highlights = map[string][]string{"name": {doc.Name}, "path": {doc.Path}}
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
		return "bm25(documents_fts) " + dir + ", d.normalized_path ASC", nil
	default:
		return "", &RequestError{Code: "invalid_sort", Message: "sort must be name, modified, size, or relevance"}
	}
}

func pathChildPrefix(path string) string {
	normalized := NormalizePath(path)
	separator := string(filepath.Separator)
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
	if err := row.Scan(&d.ID, &d.RootID, &d.Path, &d.NormalizedPath, &d.Name, &d.Extension, &d.IsFolder, &d.Size, &modified, &created, &d.Status, &d.LastSeenGeneration, &indexed, &d.ContentStatus, &d.ContentHash, &d.HashStatus, &d.Owner, &d.AccessStatus, &d.Signature); err != nil {
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

func placeholders(n int) string {
	return strings.TrimRight(strings.Repeat("?,", n), ",")
}

func escapeFTS(q string) string {
	parts := strings.Fields(q)
	for i, part := range parts {
		parts[i] = `"` + strings.ReplaceAll(part, `"`, `""`) + `"`
	}
	return strings.Join(parts, " ")
}

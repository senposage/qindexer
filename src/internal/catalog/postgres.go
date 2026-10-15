package catalog

import (
	"context"
	"strings"
	"time"
	"unicode"
)

// migratePostgres creates a fresh catalog schema. PostgreSQL catalogs are
// intentionally clean indexes; SQLite remains available for portable use.
func (c *Catalog) migratePostgres(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS documents (
			id TEXT PRIMARY KEY,
			root_id TEXT NOT NULL,
			path TEXT NOT NULL,
			normalized_path TEXT NOT NULL,
			name TEXT NOT NULL,
			extension TEXT NOT NULL,
			size BIGINT NOT NULL,
			modified_at TEXT NOT NULL,
			created_at TEXT,
			status TEXT NOT NULL,
			last_seen_generation BIGINT NOT NULL,
			last_indexed_at TEXT NOT NULL,
			content_status TEXT NOT NULL,
			ocr_status TEXT NOT NULL DEFAULT 'not_requested',
			content_error TEXT NOT NULL DEFAULT '',
			ocr_error TEXT NOT NULL DEFAULT '',
			content_attempts INTEGER NOT NULL DEFAULT 0,
			ocr_attempts INTEGER NOT NULL DEFAULT 0,
			content_retry_at TEXT NOT NULL DEFAULT '',
			ocr_retry_at TEXT NOT NULL DEFAULT '',
			content_text TEXT NOT NULL DEFAULT '',
			content_hash TEXT NOT NULL DEFAULT '',
			hash_status TEXT NOT NULL DEFAULT 'not_hashed',
			owner TEXT NOT NULL DEFAULT '',
			access_status TEXT NOT NULL DEFAULT 'metadata_readable',
			moved_from_path TEXT NOT NULL DEFAULT '',
			is_folder BOOLEAN NOT NULL DEFAULT FALSE,
			signature TEXT NOT NULL,
			missing_count INTEGER NOT NULL DEFAULT 0,
			search_vector tsvector GENERATED ALWAYS AS (
				to_tsvector('simple'::regconfig, name || ' ' || path || ' ' || extension || ' ' || content_text)
			) STORED,
			UNIQUE(root_id, normalized_path)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_root_status ON documents(root_id, status)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_extension ON documents(extension)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_modified ON documents(modified_at)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_status_kind_extension ON documents(status, is_folder, extension)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_status_kind_path ON documents(status, is_folder, normalized_path)`,
		`CREATE INDEX IF NOT EXISTS idx_documents_search_vector ON documents USING GIN(search_vector)`,
		`CREATE TABLE IF NOT EXISTS root_states (
			root_id TEXT PRIMARY KEY,
			last_generation BIGINT NOT NULL DEFAULT 0,
			last_successful_crawl_at TEXT,
			last_crawl_status TEXT NOT NULL DEFAULT 'never',
			last_error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS crawl_runs (
			id TEXT PRIMARY KEY, root_id TEXT NOT NULL, generation BIGINT NOT NULL,
			started_at TEXT NOT NULL, finished_at TEXT, status TEXT NOT NULL,
			files_seen BIGINT NOT NULL DEFAULT 0, files_added BIGINT NOT NULL DEFAULT 0,
			files_updated BIGINT NOT NULL DEFAULT 0, files_unchanged BIGINT NOT NULL DEFAULT 0,
			files_missing BIGINT NOT NULL DEFAULT 0, errors BIGINT NOT NULL DEFAULT 0,
			error_message TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS crawl_checkpoints (
			root_id TEXT NOT NULL, normalized_path TEXT NOT NULL, path TEXT NOT NULL,
			completed_at TEXT NOT NULL, PRIMARY KEY(root_id, normalized_path)
		)`,
		`CREATE TABLE IF NOT EXISTS deleted_roots (root_id TEXT PRIMARY KEY, deleted_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS folder_activity (
			root_id TEXT NOT NULL, normalized_path TEXT NOT NULL, path TEXT NOT NULL,
			score DOUBLE PRECISION NOT NULL, last_activity_at TEXT NOT NULL,
			PRIMARY KEY(root_id, normalized_path)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_folder_activity_root_score ON folder_activity(root_id, score DESC)`,
		`UPDATE documents SET content_status = 'not_indexed' WHERE content_status = 'queued'`,
		`UPDATE documents SET ocr_status = 'pending' WHERE ocr_status = 'queued'`,
		`UPDATE documents SET hash_status = 'not_hashed' WHERE hash_status = 'queued'`,
	}
	for _, statement := range statements {
		if _, err := c.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (c *Catalog) searchPostgres(ctx context.Context, req SearchRequest, maxResults int) (SearchResponse, error) {
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
	if req.Filters.Boolean.TermCount() > 100 {
		return SearchResponse{}, &RequestError{Code: "too_many_boolean_terms", Message: "at most 100 boolean terms are supported"}
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
	args, where := []any{}, []string{"d.status = 'active'"}
	hasQuery := strings.TrimSpace(req.Query) != "" || req.Filters.Boolean.TermCount() > 0
	highlightQuery := strings.Join(append(append([]string{req.Query}, req.Filters.Boolean.All...), req.Filters.Boolean.Any...), " ")
	if hasQuery {
		clause, values, err := postgresMatchWhere(req.Query, req.Filters.MatchFields, req.Filters.MatchMode, req.Filters.Boolean)
		if err != nil {
			return SearchResponse{}, err
		}
		where = append(where, clause)
		args = append(args, values...)
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
	if req.Filters.PathPrefix != "" {
		prefixes = append(prefixes, req.Filters.PathPrefix)
	}
	if len(prefixes)+len(req.Filters.ExcludePaths) > 100 {
		return SearchResponse{}, &RequestError{Code: "too_many_path_scopes", Message: "at most 100 include and exclude paths are supported"}
	}
	if len(prefixes) > 0 {
		clauses := []string{}
		for _, prefix := range prefixes {
			if prefix = strings.TrimSpace(prefix); prefix != "" {
				clauses = append(clauses, "(d.normalized_path = ? OR d.normalized_path LIKE ? ESCAPE '\\')")
				args = append(args, NormalizePath(prefix), escapeLike(pathChildPrefix(prefix))+"%")
			}
		}
		if len(clauses) > 0 {
			where = append(where, "("+strings.Join(clauses, " OR ")+")")
		}
	}
	for _, prefix := range req.Filters.ExcludePaths {
		if prefix = strings.TrimSpace(prefix); prefix != "" {
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
	switch strings.ToLower(req.Filters.Kind) {
	case "":
	case "file":
		where = append(where, "d.is_folder = FALSE")
	case "folder", "directory":
		where = append(where, "d.is_folder = TRUE")
	default:
		return SearchResponse{}, &RequestError{Code: "invalid_kind", Message: "kind must be file or folder"}
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
	sort := strings.ToLower(strings.TrimSpace(req.Sort))
	if sort == "" {
		if hasQuery {
			sort = "relevance"
		} else {
			sort = "modified"
		}
	}
	direction := strings.ToLower(strings.TrimSpace(req.SortDirection))
	if direction == "" {
		direction = strings.ToLower(strings.TrimSpace(req.Direction))
	}
	if direction == "" {
		if sort == "name" {
			direction = "asc"
		} else {
			direction = "desc"
		}
	}
	if direction != "asc" && direction != "desc" {
		return SearchResponse{}, &RequestError{Code: "invalid_sort_direction", Message: "direction must be asc or desc"}
	}
	dir := strings.ToUpper(direction)
	order := ""
	switch sort {
	case "name":
		order = "LOWER(d.name) " + dir + ", d.normalized_path ASC"
	case "modified":
		order = "d.modified_at " + dir + ", d.normalized_path ASC"
	case "size":
		order = "d.size " + dir + ", d.normalized_path ASC"
	case "relevance":
		if !hasQuery {
			return SearchResponse{}, &RequestError{Code: "invalid_sort", Message: "relevance sort requires a query"}
		}
		order = "ts_rank_cd(d.search_vector, to_tsquery('simple', ?)) " + dir + ", d.normalized_path ASC"
		args = append(args, postgresRankQuery(highlightQuery))
	default:
		return SearchResponse{}, &RequestError{Code: "invalid_sort", Message: "sort must be name, modified, size, or relevance"}
	}
	query := `SELECT d.id, d.root_id, d.path, d.normalized_path, d.name, d.extension, d.is_folder, d.size, d.modified_at, d.created_at, d.status, d.last_seen_generation, d.last_indexed_at, d.content_status, d.ocr_status, d.content_error, d.ocr_error, d.content_text, d.content_hash, d.hash_status, d.owner, d.access_status, d.moved_from_path, d.signature FROM documents d WHERE ` + strings.Join(where, " AND ") + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, limit+1, req.Offset)
	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return SearchResponse{}, err
	}
	defer rows.Close()
	docs := []Document{}
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
	response := SearchResponse{SearchID: req.SearchID, Results: docs, Offset: req.Offset, TookMS: time.Since(start).Milliseconds()}
	if len(docs) > limit {
		response.HasMore = true
		response.Results = docs[:limit]
		next := req.Offset + limit
		response.NextOffset = &next
	}
	if states, err := c.RootStates(ctx); err == nil {
		response.Index = searchIndexInfo(states, req.Filters.Roots)
	}
	return response, nil
}

// postgresMatchWhere keeps the SQL predicate and its values coupled so the
// generated PostgreSQL placeholders can never outnumber the supplied values.
func postgresMatchWhere(query string, fields []string, mode string, boolean BooleanFilter) (string, []any, error) {
	clause, values, err := postgresMatchClause(query, fields, mode, boolean)
	if err != nil {
		return "", nil, err
	}
	folderClause, folderValues, err := postgresMatchClause(query, []string{"name"}, mode, boolean)
	if err != nil {
		return "", nil, err
	}
	values = append(values, folderValues...)
	return "(" + clause + " AND (d.is_folder = FALSE OR (" + folderClause + ")))", values, nil
}

func postgresMatchClause(query string, fields []string, mode string, boolean BooleanFilter) (string, []any, error) {
	if mode == "" {
		mode = "prefix"
	}
	if mode != "prefix" && mode != "exact" {
		return "", nil, &RequestError{Code: "invalid_match_mode", Message: "match_mode supports prefix and exact"}
	}
	columns := map[string]string{"name": "d.name", "path": "d.path", "extension": "d.extension", "content": "d.content_text"}
	useSearchVector := len(fields) == 0 && mode == "prefix"
	if len(fields) == 0 {
		fields = []string{"name", "path", "extension", "content"}
	}
	selected := []string{}
	for _, field := range fields {
		column, ok := columns[strings.ToLower(strings.TrimSpace(field))]
		if !ok {
			return "", nil, &RequestError{Code: "invalid_match_fields", Message: "match_fields supports name, path, extension, and content"}
		}
		selected = append(selected, column)
	}
	predicate := func(value string) (string, []any, error) {
		value = strings.TrimSpace(value)
		if value == "" {
			return "", nil, &RequestError{Code: "invalid_boolean_filter", Message: "boolean terms must not be empty"}
		}
		if useSearchVector {
			return "(d.search_vector @@ to_tsquery('simple', ?))", []any{postgresPrefixQuery(value)}, nil
		}
		terms := []string{value}
		if mode == "prefix" {
			terms = strings.Fields(value)
		}
		clauses, args := []string{}, []any{}
		for _, term := range terms {
			options := []string{}
			for _, column := range selected {
				if mode == "exact" {
					options = append(options, "LOWER("+column+") = LOWER(?)")
					args = append(args, term)
				} else {
					options = append(options, column+" ILIKE ?")
					args = append(args, "%"+term+"%")
				}
			}
			clauses = append(clauses, "("+strings.Join(options, " OR ")+")")
		}
		return "(" + strings.Join(clauses, " AND ") + ")", args, nil
	}
	positive, args := []string{}, []any{}
	add := func(value string) error {
		clause, values, err := predicate(value)
		if err == nil {
			positive = append(positive, clause)
			args = append(args, values...)
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
		anyClauses, anyArgs := []string{}, []any{}
		for _, value := range boolean.Any {
			clause, values, err := predicate(value)
			if err != nil {
				return "", nil, err
			}
			anyClauses = append(anyClauses, clause)
			anyArgs = append(anyArgs, values...)
		}
		positive = append(positive, "("+strings.Join(anyClauses, " OR ")+")")
		args = append(args, anyArgs...)
	}
	if len(positive) == 0 {
		return "", nil, &RequestError{Code: "invalid_boolean_filter", Message: "boolean not terms require a query, all term, or any term"}
	}
	clause := "(" + strings.Join(positive, " AND ") + ")"
	for _, value := range boolean.Not {
		excluded, values, err := predicate(value)
		if err != nil {
			return "", nil, err
		}
		clause += " AND NOT " + excluded
		args = append(args, values...)
	}
	return clause, args, nil
}

func postgresPrefixQuery(value string) string {
	terms := strings.FieldsFunc(strings.ToLower(value), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	if len(terms) == 0 {
		return "''"
	}
	for i, term := range terms {
		terms[i] = term + ":*"
	}
	return strings.Join(terms, " & ")
}

func postgresRankQuery(value string) string {
	query := postgresPrefixQuery(value)
	if query == "''" {
		return "''"
	}
	return query
}

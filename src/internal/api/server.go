package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"qindexer/internal/catalog"
	"qindexer/internal/config"
	"qindexer/internal/crawler"
	"qindexer/internal/version"
	"qindexer/internal/web"
)

type Server struct {
	cfg           *config.Config
	configPath    string
	cat           *catalog.Catalog
	crawler       *crawler.Crawler
	log           *slog.Logger
	accessLog     *slog.Logger
	mu            sync.RWMutex
	searchToken   string
	adminToken    string
	instanceID    string
	searchSlots   chan struct{}
	shutdown      func()
	logPath       string
	configChanged func(*config.Config)
	resumeRoots   map[string]struct{}
	repairMu      sync.RWMutex
	repair        repairProgress
}

type repairProgress struct {
	RootID           string    `json:"root_id,omitempty"`
	Status           string    `json:"status"`
	Phase            string    `json:"phase,omitempty"`
	StartedAt        time.Time `json:"started_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at,omitempty"`
	AliasesChecked   int       `json:"aliases_checked"`
	PathsMatched     int64     `json:"paths_matched"`
	PathsProcessed   int64     `json:"paths_processed"`
	PathsRewritten   int64     `json:"paths_rewritten"`
	DuplicatesMerged int64     `json:"duplicate_paths_merged"`
	Error            string    `json:"error,omitempty"`
}

const maxRequestBodyBytes = 1 << 20

type rootAliasView struct {
	AliasID   string `json:"alias_id"`
	Platform  string `json:"platform"`
	Path      string `json:"path"`
	Target    string `json:"target,omitempty"`
	Canonical bool   `json:"canonical"`
}

func New(cfg *config.Config, configPath string, cat *catalog.Catalog, cr *crawler.Crawler, log *slog.Logger, searchToken, adminToken string) *Server {
	return &Server{cfg: cfg, configPath: configPath, cat: cat, crawler: cr, log: log, searchToken: searchToken, adminToken: adminToken, instanceID: uuid.NewString(), searchSlots: make(chan struct{}, 32), resumeRoots: map[string]struct{}{}}
}

func (s *Server) SetShutdown(shutdown func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdown = shutdown
}

func (s *Server) SetLogPath(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logPath = path
}

// SetAccessLogger keeps routine HTTP access records out of the operational
// log, where crawler, storage, and extraction failures need to stay visible.
func (s *Server) SetAccessLogger(log *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accessLog = log
}

// SetConfigChanged publishes durable configuration changes to long-running
// components which keep their own immutable snapshots.
func (s *Server) SetConfigChanged(callback func(*config.Config)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.configChanged = callback
}

// saveConfigLocked persists first, then makes the candidate observable. The
// caller must hold s.mu so failed writes cannot change runtime behavior.
func (s *Server) saveConfigLocked(candidate *config.Config) error {
	if err := config.Save(s.configPath, candidate); err != nil {
		return err
	}
	// Keep the original pointer stable for embedding callers, while crawler and
	// watcher receive separate immutable snapshots below.
	*s.cfg = *config.Clone(candidate)
	s.crawler.ApplyConfig(s.cfg)
	if s.configChanged != nil {
		s.configChanged(s.cfg)
	}
	return nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}

func (s *Server) SearchHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.withSearchAuth(s.health))
	mux.HandleFunc("GET /v1/capabilities", s.withSearchAuth(s.capabilities))
	mux.HandleFunc("GET /v1/roots", s.withSearchAuth(s.roots))
	mux.HandleFunc("POST /v1/search", s.withSearchAuth(s.search))
	mux.HandleFunc("POST /v1/directories", s.withSearchAuth(s.directories))
	mux.HandleFunc("POST /v1/suggest", s.withSearchAuth(s.suggest))
	return s.requestLog(mux)
}

func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	s.mountAdminUI(mux)
	mux.HandleFunc("POST /admin/v1/bootstrap", s.bootstrapAdmin)
	// The admin UI needs a small unauthenticated status view. Keep this payload
	// deliberately service-level: configuration and root details stay protected.
	mux.HandleFunc("GET /admin/v1/status", s.publicStatus)
	mux.HandleFunc("GET /admin/v1/service", s.withAdminAuth(s.health))
	mux.HandleFunc("GET /admin/v1/config", s.withAdminAuth(s.configView))
	mux.HandleFunc("GET /admin/v1/config/export", s.withAdminAuth(s.exportConfig))
	mux.HandleFunc("POST /admin/v1/config/import", s.withAdminAuth(s.importConfig))
	mux.HandleFunc("POST /admin/v1/config/validate", s.withAdminAuth(s.configValidate))
	mux.HandleFunc("GET /admin/v1/metrics", s.withAdminAuth(s.metrics))
	mux.HandleFunc("GET /admin/v1/diagnostics", s.withAdminAuth(s.diagnostics))
	mux.HandleFunc("GET /admin/v1/logs", s.withAdminAuth(s.logs))
	mux.HandleFunc("POST /admin/v1/crawler/pause", s.withAdminAuth(s.pauseCrawler))
	mux.HandleFunc("POST /admin/v1/crawler/resume", s.withAdminAuth(s.resumeCrawler))
	mux.HandleFunc("POST /admin/v1/crawler/stop", s.withAdminAuth(s.stopCrawler))
	mux.HandleFunc("POST /admin/v1/service/stop", s.withAdminAuth(s.stopService))
	mux.HandleFunc("PUT /admin/v1/crawler/settings", s.withAdminAuth(s.updateCrawlerSettings))
	mux.HandleFunc("PUT /admin/v1/network", s.withAdminAuth(s.updateNetworkSettings))
	mux.HandleFunc("GET /admin/v1/roots", s.withAdminAuth(s.roots))
	mux.HandleFunc("POST /admin/v1/roots", s.withAdminAuth(s.createRoot))
	mux.HandleFunc("DELETE /admin/v1/roots/{root_id}", s.withAdminAuth(s.deleteRoot))
	mux.HandleFunc("POST /admin/v1/roots/{root_id}/crawl", s.withAdminAuth(s.crawlRoot))
	mux.HandleFunc("POST /admin/v1/roots/{root_id}/clear-index", s.withAdminAuth(s.clearRootIndex))
	mux.HandleFunc("POST /admin/v1/roots/{root_id}/repair-index", s.withAdminAuth(s.repairRootIndex))
	mux.HandleFunc("GET /admin/v1/operations/repair", s.withAdminAuth(s.repairStatus))
	mux.HandleFunc("POST /admin/v1/roots/{root_id}/validate", s.withAdminAuth(s.validateRoot))
	mux.HandleFunc("PUT /admin/v1/roots/{root_id}/rules", s.withAdminAuth(s.updateRootRules))
	mux.HandleFunc("GET /admin/v1/crawls", s.withAdminAuth(s.crawls))
	return s.requestLog(mux)
}

func (s *Server) publicStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	adminConfigured := s.adminToken != ""
	s.mu.RUnlock()

	states, err := s.cat.RootStates(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":           "degraded",
			"protocol_version": "1.1",
			"service_instance": s.instanceID,
			"version":          version.Version,
			"admin_configured": adminConfigured,
			"index":            map[string]any{"ready": false, "document_count": 0},
		})
		return
	}
	var count int64
	for _, state := range states {
		count += state.DocumentCount
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"protocol_version": "1.1",
		"service_instance": s.instanceID,
		"version":          version.Version,
		"admin_configured": adminConfigured,
		"index":            map[string]any{"ready": true, "document_count": count},
	})
}

func (s *Server) mountAdminUI(mux *http.ServeMux) {
	static, err := fs.Sub(web.Static, "static")
	if err != nil {
		s.log.Warn("admin ui unavailable", "error", err)
		return
	}
	files := http.FileServer(http.FS(static))
	noCache := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			next.ServeHTTP(w, r)
		})
	}
	mux.Handle("GET /static/", noCache(http.StripPrefix("/static/", files)))
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		http.ServeFileFS(w, r, static, "index.html")
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		serveIndex(w, r)
	})
	mux.HandleFunc("GET /config", serveIndex)
	mux.HandleFunc("GET /roots", serveIndex)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	states, err := s.cat.RootStates(r.Context())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status": "degraded", "protocol_version": "1.1", "service_instance": s.instanceID,
			"version": version.Version, "commit": version.Commit,
			"index": map[string]any{"ready": false, "document_count": 0, "error": "catalog unavailable"},
		})
		return
	}
	var count int64
	for _, state := range states {
		count += state.DocumentCount
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "ok",
		"protocol_version": "1.1",
		"service_instance": s.instanceID,
		"version":          version.Version,
		"commit":           version.Commit,
		"generation":       maxGeneration(states),
		"index": map[string]any{
			"ready":          true,
			"document_count": count,
		},
	})
}

func (s *Server) capabilities(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cfg := config.Clone(s.cfg)
	s.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"protocol_version": "1.1",
		"service_instance": s.instanceID,
		"service_version":  version.Version,
		"features": map[string]bool{
			"metadata_search": true,
			"content_search": anyRootUses(cfg, func(root config.RootConfig) bool {
				return root.ContentExtractionEnabled(cfg.Crawler.ContentExtraction.Enabled)
			}),
			"ocr_search": anyRootUses(cfg, func(root config.RootConfig) bool {
				return root.ContentExtractionEnabled(cfg.Crawler.ContentExtraction.Enabled) && root.OCREnabled(cfg.Crawler.OCR.Enabled)
			}),
			"acl_filtering":   false,
			"content_hashing": anyRootUses(cfg, func(root config.RootConfig) bool { return root.HashingEnabled(cfg.Crawler.Hashing.Enabled) }),
			"crawl_control":   true,
			"folder_search":   true,
			"folder_suggest":  true,
			"pagination":      true,
			"sorting":         true,
			"path_scopes":     true,
			"root_filtering":  true,
			"exclusions":      true,
		},
		"supported_filters": []string{
			"roots",
			"extensions",
			"path_prefix",
			"path_prefixes",
			"include_paths",
			"exclude_paths",
			"scope_aliases",
			"kind",
			"modified_after",
			"modified_before",
			"min_size",
			"max_size",
			"match_fields",
		},
		"limits": map[string]any{
			"max_page_size":           cfg.Index.MaxResults,
			"max_path_scopes":         100,
			"max_roots":               100,
			"max_extensions":          100,
			"max_concurrent_searches": cap(s.searchSlots),
		},
		"supported_sorts": []string{"name", "modified", "size", "relevance"},
	})
}

func (s *Server) roots(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	roots := append([]config.RootConfig(nil), s.cfg.Roots...)
	s.mu.RUnlock()
	states, err := s.cat.RootStates(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "catalog_error", err.Error())
		return
	}
	s.repairMu.RLock()
	repair := s.repair
	s.repairMu.RUnlock()
	type rootView struct {
		config.RootConfig
		FriendlyName          string          `json:"friendly_name"`
		CanonicalPath         string          `json:"canonical_path"`
		Available             bool            `json:"available"`
		LastSuccessfulCrawlAt *time.Time      `json:"last_successful_crawl_at,omitempty"`
		LastStatus            string          `json:"last_status"`
		LastError             string          `json:"last_error,omitempty"`
		DocumentCount         int64           `json:"document_count"`
		MissingCount          int64           `json:"missing_count"`
		Aliases               []rootAliasView `json:"aliases"`
	}
	out := []rootView{}
	for _, root := range roots {
		state := states[root.ID]
		status := state.LastCrawlStatus
		if status == "" {
			status = "never"
		}
		if repair.Status == "running" && repair.RootID == root.ID {
			status = "repairing"
		}
		out = append(out, rootView{
			RootConfig:   root,
			FriendlyName: root.FriendlyName(), CanonicalPath: root.Path, Available: status != "unreachable",
			LastSuccessfulCrawlAt: state.LastSuccessfulCrawlAt, LastStatus: status,
			LastError: state.LastError, DocumentCount: state.DocumentCount,
			MissingCount: state.MissingCount,
			Aliases:      rootAliases(root),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"roots": out})
}

func anyRootUses(cfg *config.Config, include func(config.RootConfig) bool) bool {
	for _, root := range cfg.Roots {
		if root.Enabled && include(root) {
			return true
		}
	}
	return false
}

func rootAliases(root config.RootConfig) []rootAliasView {
	aliases := make([]rootAliasView, 0, len(root.PathAliases)+1)
	aliases = append(aliases, rootAliasView{AliasID: root.ID + ":canonical", Platform: "service", Path: root.Path, Canonical: true})
	for _, alias := range root.PathAliases {
		id := strings.TrimSpace(alias.ID)
		if id == "" {
			sum := sha256.Sum256([]byte(root.ID + "\x00" + alias.Platform + "\x00" + alias.Path))
			id = fmt.Sprintf("%s:%x", root.ID, sum[:8])
		}
		aliases = append(aliases, rootAliasView{AliasID: id, Platform: alias.Platform, Path: alias.Path, Target: strings.TrimSpace(alias.Target)})
	}
	return aliases
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	var req catalog.SearchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.SearchID == "" {
		req.SearchID = uuid.NewString()
	}
	resolution, err := s.resolveScopeAliases(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path_scope", err.Error())
		return
	}
	s.mu.RLock()
	maxResults := s.cfg.Index.MaxResults
	s.mu.RUnlock()
	if !s.acquireSearchSlot(w) {
		return
	}
	defer s.releaseSearchSlot()
	resp, err := s.cat.Search(r.Context(), req, maxResults)
	if err != nil {
		if requestErr, ok := err.(*catalog.RequestError); ok {
			writeError(w, http.StatusBadRequest, requestErr.Code, requestErr.Message)
			return
		}
		writeError(w, http.StatusServiceUnavailable, "search_unavailable", err.Error())
		return
	}
	resolution.applyDisplayAliases(&resp)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) directories(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	maxResults := s.cfg.Index.MaxResults
	s.mu.RUnlock()
	s.searchWithKind(w, r, "folder", maxResults)
}

func (s *Server) suggest(w http.ResponseWriter, r *http.Request) {
	limit := 25
	s.searchWithKind(w, r, "folder", limit)
}

func (s *Server) acquireSearchSlot(w http.ResponseWriter) bool {
	select {
	case s.searchSlots <- struct{}{}:
		return true
	default:
		writeError(w, http.StatusTooManyRequests, "search_busy", "search capacity is temporarily exhausted")
		return false
	}
}

func (s *Server) releaseSearchSlot() { <-s.searchSlots }

func (s *Server) searchWithKind(w http.ResponseWriter, r *http.Request, kind string, maxResults int) {
	var req catalog.SearchRequest
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.SearchID == "" {
		req.SearchID = uuid.NewString()
	}
	resolution, err := s.resolveScopeAliases(&req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path_scope", err.Error())
		return
	}
	req.Filters.Kind = kind
	if req.Limit <= 0 || req.Limit > maxResults {
		req.Limit = maxResults
	}
	if !s.acquireSearchSlot(w) {
		return
	}
	defer s.releaseSearchSlot()
	resp, err := s.cat.Search(r.Context(), req, maxResults)
	if err != nil {
		if requestErr, ok := err.(*catalog.RequestError); ok {
			writeError(w, http.StatusBadRequest, requestErr.Code, requestErr.Message)
			return
		}
		writeError(w, http.StatusServiceUnavailable, "search_unavailable", err.Error())
		return
	}
	resolution.applyDisplayAliases(&resp)
	writeJSON(w, http.StatusOK, resp)
}

type scopeMatch struct {
	rootID string
	path   string
}

type displayAlias struct {
	rootID        string
	canonicalPath string
	displayPath   string
	priority      int
}

type scopeResolution struct {
	displayAliases []displayAlias
}

func (r scopeResolution) applyDisplayAliases(resp *catalog.SearchResponse) {
	for i := range resp.Results {
		bestLength := -1
		bestPriority := -1
		for _, alias := range r.displayAliases {
			doc := &resp.Results[i]
			if doc.RootID != alias.rootID {
				continue
			}
			suffix, ok := scopedPathSuffix(doc.Path, alias.canonicalPath)
			if !ok || (len(alias.canonicalPath) < bestLength || (len(alias.canonicalPath) == bestLength && alias.priority <= bestPriority)) {
				continue
			}
			doc.DisplayPath = joinCanonicalScope(alias.displayPath, suffix)
			bestLength = len(alias.canonicalPath)
			bestPriority = alias.priority
		}
	}
}

func (s *Server) resolveScopeAliases(req *catalog.SearchRequest) (scopeResolution, error) {
	s.mu.RLock()
	roots := append([]config.RootConfig(nil), s.cfg.Roots...)
	s.mu.RUnlock()
	resolution := scopeResolution{}
	requestedRoots := map[string]bool{}
	for _, id := range req.Filters.Roots {
		requestedRoots[id] = true
	}
	resolvedRoots := map[string]bool{}
	allowedRoot := func(rootID string) bool {
		return len(requestedRoots) == 0 || requestedRoots[rootID]
	}
	for _, root := range roots {
		if !allowedRoot(root.ID) {
			continue
		}
		for _, alias := range root.PathAliases {
			aliasPath := strings.TrimSpace(alias.Path)
			if aliasPath == "" {
				continue
			}
			resolution.displayAliases = append(resolution.displayAliases, displayAlias{rootID: root.ID, canonicalPath: aliasPrimaryCanonicalTarget(root.Path, alias), displayPath: aliasPath, priority: aliasDisplayPriority(alias)})
		}
	}
	unique := func(scope string, matches []scopeMatch) (scopeMatch, bool, error) {
		if len(matches) == 0 {
			return scopeMatch{}, false, nil
		}
		first := matches[0]
		for _, match := range matches[1:] {
			if match.rootID != first.rootID || !sameScopePath(match.path, first.path) {
				return scopeMatch{}, false, fmt.Errorf("scope %q matches more than one root alias", scope)
			}
		}
		return first, true, nil
	}
	configuredMatches := func(scope string, aliasesOnly bool) []scopeMatch {
		matches := []scopeMatch{}
		for _, root := range roots {
			if !allowedRoot(root.ID) {
				continue
			}
			for _, alias := range root.PathAliases {
				if suffix, ok := scopedPathSuffix(scope, alias.Path); ok {
					matches = append(matches, scopeMatch{rootID: root.ID, path: joinCanonicalScope(aliasPrimaryCanonicalTarget(root.Path, alias), suffix)})
				}
			}
			if !aliasesOnly {
				if suffix, ok := scopedPathSuffix(scope, root.Path); ok {
					matches = append(matches, scopeMatch{rootID: root.ID, path: joinCanonicalScope(root.Path, suffix)})
				}
			}
		}
		return matches
	}
	resolve := func(scope string) (string, error) {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			return scope, nil
		}
		configuredAliasMatches := configuredMatches(scope, true)
		if match, found, err := unique(scope, configuredAliasMatches); err != nil || found {
			if err != nil {
				return "", err
			}
			resolvedRoots[match.rootID] = true
			return match.path, nil
		}

		var aliasMatches []struct {
			match   scopeMatch
			display string
		}
		for _, alias := range req.Filters.ScopeAliases {
			if suffix, ok := scopedPathSuffix(scope, alias.Path); ok {
				target := joinCanonicalScope(alias.Target, suffix)
				matches := configuredMatches(target, false)
				match, found, err := unique(scope, matches)
				if err != nil {
					return "", err
				}
				if found {
					aliasMatches = append(aliasMatches, struct {
						match   scopeMatch
						display string
					}{match: match, display: joinCanonicalScope(alias.Path, suffix)})
				}
			}
		}
		if len(aliasMatches) > 0 {
			first := aliasMatches[0]
			for _, candidate := range aliasMatches[1:] {
				if candidate.match.rootID != first.match.rootID || !sameScopePath(candidate.match.path, first.match.path) {
					return "", fmt.Errorf("scope %q matches more than one request alias", scope)
				}
			}
			resolvedRoots[first.match.rootID] = true
			resolution.displayAliases = append(resolution.displayAliases, displayAlias{rootID: first.match.rootID, canonicalPath: first.match.path, displayPath: first.display, priority: 1000})
			return first.match.path, nil
		}

		matches := configuredMatches(scope, false)
		match, found, err := unique(scope, matches)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("scope %q does not match a configured root or alias", scope)
		}
		resolvedRoots[match.rootID] = true
		return match.path, nil
	}
	var err error
	if req.Filters.PathPrefix, err = resolve(req.Filters.PathPrefix); err != nil {
		return scopeResolution{}, err
	}
	for i := range req.Filters.PathPrefixes {
		if req.Filters.PathPrefixes[i], err = resolve(req.Filters.PathPrefixes[i]); err != nil {
			return scopeResolution{}, err
		}
	}
	for i := range req.Filters.IncludePaths {
		if req.Filters.IncludePaths[i], err = resolve(req.Filters.IncludePaths[i]); err != nil {
			return scopeResolution{}, err
		}
	}
	for i := range req.Filters.ExcludePaths {
		if req.Filters.ExcludePaths[i], err = resolve(req.Filters.ExcludePaths[i]); err != nil {
			return scopeResolution{}, err
		}
	}
	if len(resolvedRoots) > 0 {
		req.Filters.Roots = make([]string, 0, len(resolvedRoots))
		for _, root := range roots {
			if root.Enabled && resolvedRoots[root.ID] {
				req.Filters.Roots = append(req.Filters.Roots, root.ID)
			}
		}
	} else {
		req.Filters.Roots = configuredSearchRootIDs(roots, requestedRoots)
	}
	return resolution, nil
}

func configuredSearchRootIDs(roots []config.RootConfig, requested map[string]bool) []string {
	ids := make([]string, 0, len(roots))
	for _, root := range roots {
		if !root.Enabled {
			continue
		}
		if len(requested) > 0 && !requested[root.ID] {
			continue
		}
		ids = append(ids, root.ID)
	}
	if len(ids) == 0 {
		return []string{"__qindexer_no_enabled_roots__"}
	}
	return ids
}

func scopedPathSuffix(path, prefix string) (string, bool) {
	windowsStyle := isWindowsStylePath(path) || isWindowsStylePath(prefix)
	path, prefix = strings.ReplaceAll(path, "\\", "/"), strings.ReplaceAll(prefix, "\\", "/")
	prefix = strings.TrimRight(prefix, "/")
	if prefix == "" {
		return "", false
	}
	if scopePathEqual(path, prefix, windowsStyle) {
		return "", true
	}
	if len(path) > len(prefix) && scopePathEqual(path[:len(prefix)], prefix, windowsStyle) && path[len(prefix)] == '/' {
		return path[len(prefix)+1:], true
	}
	return "", false
}

func joinCanonicalScope(root, suffix string) string {
	if suffix == "" {
		return root
	}
	if isWindowsStylePath(root) {
		return strings.TrimRight(root, "\\/") + "\\" + strings.ReplaceAll(suffix, "/", "\\")
	}
	return strings.TrimRight(root, "\\/") + "/" + strings.ReplaceAll(suffix, "\\", "/")
}

func sameScopePath(a, b string) bool {
	windowsStyle := isWindowsStylePath(a) || isWindowsStylePath(b)
	return scopePathEqual(strings.ReplaceAll(a, "\\", "/"), strings.ReplaceAll(b, "\\", "/"), windowsStyle)
}

func isWindowsStylePath(value string) bool {
	value = strings.TrimSpace(value)
	return strings.Contains(value, "\\") || (len(value) >= 2 && value[1] == ':')
}

func scopePathEqual(a, b string, windowsStyle bool) bool {
	if windowsStyle {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func (s *Server) crawlRoot(w http.ResponseWriter, r *http.Request) {
	rootID := r.PathValue("root_id")
	s.mu.RLock()
	configured := false
	for _, root := range s.cfg.Roots {
		if root.ID == rootID {
			configured = true
			break
		}
	}
	s.mu.RUnlock()
	if !configured {
		writeError(w, http.StatusNotFound, "root_not_found", "root not found")
		return
	}
	if s.crawler.InMaintenance() {
		writeError(w, http.StatusConflict, "crawler_maintenance", "crawler maintenance is in progress; retry after it completes")
		return
	}
	if s.crawler.IsRootRunning(rootID) {
		writeError(w, http.StatusConflict, "crawl_already_running", "this root is already crawling")
		return
	}
	s.log.Info("manual root crawl requested", "root", rootID)
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if _, err := s.crawler.CrawlRoot(ctx, rootID); err != nil {
			s.log.Warn("manual crawl failed", "root", rootID, "error", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "scheduled", "root_id": rootID})
}

func (s *Server) rememberResumeRoots(rootIDs []string) {
	if len(rootIDs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rootID := range rootIDs {
		if rootID != "" {
			s.resumeRoots[rootID] = struct{}{}
		}
	}
}

func (s *Server) takeResumeRoots() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	roots := make([]string, 0, len(s.resumeRoots))
	for rootID := range s.resumeRoots {
		roots = append(roots, rootID)
	}
	s.resumeRoots = map[string]struct{}{}
	return roots
}

func (s *Server) scheduleRootCrawls(rootIDs []string, reason string) {
	for _, rootID := range rootIDs {
		rootID := rootID
		go func() {
			if _, err := s.crawler.CrawlRoot(context.Background(), rootID); err != nil {
				s.log.Warn("resumed root crawl failed", "root", rootID, "reason", reason, "error", err)
			}
		}()
	}
}

func (s *Server) clearRootIndex(w http.ResponseWriter, r *http.Request) {
	rootID := r.PathValue("root_id")
	s.log.Info("root clear-index requested", "root", rootID)
	var req struct {
		ConfirmRootID string `json:"confirm_root_id"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.ConfirmRootID != rootID {
		writeError(w, http.StatusBadRequest, "clear_confirmation_required", "confirm_root_id must match the root being cleared")
		return
	}
	s.mu.RLock()
	known := false
	for _, root := range s.cfg.Roots {
		if root.ID == rootID {
			known = true
			break
		}
	}
	s.mu.RUnlock()
	if !known {
		s.log.Warn("root clear-index failed; root not found", "root", rootID)
		writeError(w, http.StatusNotFound, "root_not_found", "root not found")
		return
	}
	if !s.stopRootCrawl(r.Context(), rootID) {
		s.log.Warn("root clear-index could not stop running crawl", "root", rootID)
		writeError(w, http.StatusConflict, "root_still_stopping", "root crawl is still stopping; retry shortly")
		return
	}
	removed, err := s.cat.ClearRoot(r.Context(), rootID)
	if err != nil {
		s.log.Error("root clear-index failed", "root", rootID, "error", err)
		writeError(w, http.StatusInternalServerError, "catalog_clear_failed", err.Error())
		return
	}
	s.log.Info("root clear-index completed", "root", rootID, "documents_removed", removed)
	writeJSON(w, http.StatusOK, map[string]any{"status": "cleared", "root_id": rootID, "documents_removed": removed})
}

func (s *Server) repairRootIndex(w http.ResponseWriter, r *http.Request) {
	rootID := r.PathValue("root_id")
	s.log.Info("root index repair requested", "root", rootID)
	s.setRepairProgress(repairProgress{RootID: rootID, Status: "running", Phase: "validating root", StartedAt: time.Now().UTC()})
	s.mu.RLock()
	var root config.RootConfig
	found := false
	for _, candidate := range s.cfg.Roots {
		if candidate.ID == rootID {
			root = candidate
			found = true
			break
		}
	}
	s.mu.RUnlock()
	if !found {
		s.finishRepair(rootID, "failed", "root not found")
		s.log.Warn("root index repair failed; root not found", "root", rootID)
		writeError(w, http.StatusNotFound, "root_not_found", "root not found")
		return
	}
	if strings.TrimSpace(root.Path) == "" {
		s.finishRepair(rootID, "failed", "root path is required")
		s.log.Warn("root index repair failed; empty root path", "root", rootID)
		writeError(w, http.StatusBadRequest, "invalid_root_path", "root path is required")
		return
	}
	resume, activeRoots, ok := s.stopAllCrawlsForMaintenance("root_index_repair", rootID)
	if !ok {
		s.finishRepair(rootID, "failed", "crawler is still stopping")
		s.log.Warn("root index repair could not stop active crawls", "root", rootID, "active_roots", activeRoots)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": map[string]any{
				"code":        "crawler_still_stopping",
				"message":     "crawler is still stopping; retry shortly",
				"retryable":   true,
				"unavailable": false,
			},
			"active_roots": activeRoots,
		})
		return
	}
	defer func() {
		resume()
		s.scheduleRootCrawls(activeRoots, "root_index_repair")
	}()
	var rewritten, merged int64
	var embeddedRewritten, embeddedMerged int64
	repairedAliases := []string{}
	checkedAliases := 0
	for _, alias := range root.PathAliases {
		aliasPath := strings.TrimSpace(alias.Path)
		if aliasPath == "" || samePathAlias(aliasPath, root.Path) {
			continue
		}
		checkedAliases++
		s.updateRepairProgress(rootID, "rewriting configured aliases", checkedAliases, 0, 0, rewritten, merged)
		for _, target := range aliasCanonicalTargets(root.Path, alias) {
			s.log.Debug("root index repair checking alias", "root", rootID, "alias", aliasPath, "canonical", target)
			nextRewritten, nextMerged, err := s.cat.RewriteRootPathWithProgress(r.Context(), rootID, aliasPath, target, func(matched, processed int64) {
				s.updateRepairProgress(rootID, "rewriting configured aliases", checkedAliases, matched, processed, rewritten, merged)
			})
			if err != nil {
				s.finishRepair(rootID, "failed", err.Error())
				s.log.Error("root index repair failed", "root", rootID, "alias", aliasPath, "canonical", target, "error", err)
				writeError(w, http.StatusInternalServerError, "root_path_repair_failed", err.Error())
				return
			}
			if nextRewritten > 0 || nextMerged > 0 {
				repairedAliases = append(repairedAliases, aliasPath)
			}
			rewritten += nextRewritten
			merged += nextMerged
		}
	}
	s.updateRepairProgress(rootID, "repairing embedded paths", checkedAliases, 0, 0, rewritten, merged)
	embeddedRewritten, embeddedMerged, err := s.cat.RepairEmbeddedRootPath(r.Context(), rootID, root.Path)
	if err != nil {
		s.finishRepair(rootID, "failed", err.Error())
		s.log.Error("root embedded-path repair failed", "root", rootID, "canonical", root.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "root_path_repair_failed", err.Error())
		return
	}
	rewritten += embeddedRewritten
	merged += embeddedMerged
	removedPrevious, err := s.removePreviousPathAliases(rootID)
	if err != nil {
		s.finishRepair(rootID, "failed", err.Error())
		s.log.Error("root index repair alias cleanup failed", "root", rootID, "error", err)
		writeError(w, http.StatusInternalServerError, "root_alias_cleanup_failed", err.Error())
		return
	}
	s.updateRepairProgress(rootID, "completed", checkedAliases, 0, 0, rewritten, merged)
	s.finishRepair(rootID, "completed", "")
	s.log.Info("root index repair completed", "root", rootID, "aliases_checked", checkedAliases, "paths_rewritten", rewritten, "duplicate_paths_merged", merged, "embedded_paths_rewritten", embeddedRewritten, "embedded_paths_merged", embeddedMerged, "previous_aliases_removed", removedPrevious)
	writeJSON(w, http.StatusOK, map[string]any{"status": "repaired", "root_id": rootID, "aliases_checked": checkedAliases, "paths_rewritten": rewritten, "duplicate_paths_merged": merged, "aliases_repaired": repairedAliases, "embedded_paths_rewritten": embeddedRewritten, "embedded_paths_merged": embeddedMerged, "previous_aliases_removed": removedPrevious})
}

func (s *Server) repairStatus(w http.ResponseWriter, r *http.Request) {
	s.repairMu.RLock()
	progress := s.repair
	s.repairMu.RUnlock()
	writeJSON(w, http.StatusOK, progress)
}

func (s *Server) setRepairProgress(progress repairProgress) {
	progress.UpdatedAt = time.Now().UTC()
	s.repairMu.Lock()
	s.repair = progress
	s.repairMu.Unlock()
}

func (s *Server) updateRepairProgress(rootID, phase string, aliases int, matched, processed, rewritten, merged int64) {
	s.repairMu.Lock()
	if s.repair.RootID == rootID && s.repair.Status == "running" {
		s.repair.Phase, s.repair.AliasesChecked = phase, aliases
		s.repair.PathsMatched, s.repair.PathsProcessed = matched, processed
		s.repair.PathsRewritten, s.repair.DuplicatesMerged = rewritten, merged
		s.repair.UpdatedAt = time.Now().UTC()
	}
	s.repairMu.Unlock()
}

func (s *Server) finishRepair(rootID, status, message string) {
	s.repairMu.Lock()
	if s.repair.RootID == rootID {
		s.repair.Status, s.repair.Error = status, message
		s.repair.UpdatedAt = time.Now().UTC()
	}
	s.repairMu.Unlock()
}

func (s *Server) deleteRoot(w http.ResponseWriter, r *http.Request) {
	rootID := r.PathValue("root_id")
	s.log.Info("root delete requested", "root", rootID)
	var req struct {
		ConfirmRootID string `json:"confirm_root_id"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.ConfirmRootID != rootID {
		writeError(w, http.StatusBadRequest, "delete_confirmation_required", "confirm_root_id must match the root being deleted")
		return
	}
	if !s.stopRootCrawl(r.Context(), rootID) {
		s.log.Warn("root delete could not stop running crawl", "root", rootID)
		writeError(w, http.StatusConflict, "root_still_stopping", "root crawl is still stopping; retry shortly")
		return
	}
	s.mu.Lock()
	index := -1
	for i, root := range s.cfg.Roots {
		if root.ID == rootID {
			index = i
			break
		}
	}
	if index < 0 {
		s.mu.Unlock()
		s.log.Warn("root delete failed; root not found", "root", rootID)
		writeError(w, http.StatusNotFound, "root_not_found", "root not found")
		return
	}
	candidate := config.Clone(s.cfg)
	candidate.Roots = append([]config.RootConfig(nil), candidate.Roots[:index]...)
	candidate.Roots = append(candidate.Roots, s.cfg.Roots[index+1:]...)
	if err := s.saveConfigLocked(candidate); err != nil {
		s.mu.Unlock()
		s.log.Error("root delete config save failed", "root", rootID, "error", err)
		writeError(w, http.StatusInternalServerError, "config_save_failed", err.Error())
		return
	}
	s.mu.Unlock()
	removed, err := s.cat.DeactivateRoot(r.Context(), rootID)
	if err != nil {
		s.log.Error("root delete catalog cleanup deferred", "root", rootID, "error", err)
		writeError(w, http.StatusServiceUnavailable, "catalog_cleanup_failed", err.Error())
		return
	}
	s.log.Info("root delete completed", "root", rootID, "documents_marked_deleted", removed)
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "root_id": rootID, "documents_removed": removed, "cleanup": "deferred"})
}

func (s *Server) stopRootCrawl(ctx context.Context, rootID string) bool {
	if s.crawler.IsRootRunning(rootID) {
		s.crawler.CancelRoot(rootID)
	} else {
		s.crawler.CancelEnrichment()
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.crawler.WaitRoot(waitCtx, rootID) && s.crawler.WaitEnrichment(waitCtx)
}

func (s *Server) removePreviousPathAliases(rootID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Roots {
		if s.cfg.Roots[i].ID != rootID {
			continue
		}
		kept := make([]config.PathAlias, 0, len(s.cfg.Roots[i].PathAliases))
		removed := 0
		for _, alias := range s.cfg.Roots[i].PathAliases {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(alias.ID)), "previous-") {
				removed++
				continue
			}
			kept = append(kept, alias)
		}
		if removed == 0 {
			return 0, nil
		}
		candidate := config.Clone(s.cfg)
		candidate.Roots[i].PathAliases = kept
		if err := s.saveConfigLocked(candidate); err != nil {
			return 0, err
		}
		return removed, nil
	}
	return 0, nil
}

func (s *Server) stopAllCrawlsForMaintenance(operation, rootID string) (func(), []string, bool) {
	until, resume := s.crawler.BeginMaintenance(10 * time.Minute)
	stats := s.crawler.Stats()
	s.log.Info("crawler maintenance stop requested", "operation", operation, "root", rootID, "active_crawls", stats.ActiveCrawls, "paused_until", until)
	interruptedRoots := s.crawler.CancelAll()
	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stopped := s.crawler.WaitAll(waitCtx)
	if !stopped {
		activeRoots := s.crawler.ActiveRoots()
		s.log.Warn("crawler maintenance stop timed out", "operation", operation, "root", rootID, "active_roots", activeRoots)
		resume()
		s.log.Info("crawler maintenance released after timeout", "operation", operation, "root", rootID)
		return func() {}, activeRoots, false
	}
	s.log.Info("crawler maintenance stop completed", "operation", operation, "root", rootID)
	return resume, interruptedRoots, true
}

func (s *Server) validateRoot(w http.ResponseWriter, r *http.Request) {
	rootID := r.PathValue("root_id")
	s.log.Debug("root validation requested", "root", rootID)
	s.mu.RLock()
	roots := append([]config.RootConfig(nil), s.cfg.Roots...)
	s.mu.RUnlock()
	for _, root := range roots {
		if root.ID == rootID {
			err := validatePath(root.Path)
			if err != nil {
				s.log.Warn("root validation unreachable", "root", rootID, "path", root.Path, "error", err)
				writeJSON(w, http.StatusOK, map[string]any{"root_id": rootID, "status": "unreachable", "error": err.Error()})
				return
			}
			expanded, _ := crawler.ExpandRootPaths(root.Path)
			s.log.Info("root validation reachable", "root", rootID, "path", root.Path, "expanded_count", len(expanded))
			writeJSON(w, http.StatusOK, map[string]any{"root_id": rootID, "status": "reachable", "expanded_paths": expanded, "expanded_count": len(expanded)})
			return
		}
	}
	writeError(w, http.StatusNotFound, "root_not_found", "root not found")
}

type rootRulesUpdate struct {
	ID                    string             `json:"id"`
	Name                  string             `json:"name"`
	Path                  string             `json:"path"`
	Enabled               bool               `json:"enabled"`
	Labels                []string           `json:"labels"`
	IncludeExtensions     []string           `json:"include_extensions"`
	ExcludeExtensions     []string           `json:"exclude_extensions"`
	IncludeFilePatterns   []string           `json:"include_file_patterns"`
	ExcludeFilePatterns   []string           `json:"exclude_file_patterns"`
	IncludeFolderPatterns []string           `json:"include_folder_patterns"`
	ExcludeFolderPatterns []string           `json:"exclude_folder_patterns"`
	ExcludePatterns       []string           `json:"exclude_patterns"`
	CredentialRef         string             `json:"credential_ref"`
	PathAliases           []config.PathAlias `json:"path_aliases"`
	ContentExtraction     *bool              `json:"content_extraction"`
	OCR                   *bool              `json:"ocr"`
	Hashing               *bool              `json:"hashing"`
	CollectOwnership      *bool              `json:"collect_ownership"`
}

func (s *Server) createRoot(w http.ResponseWriter, r *http.Request) {
	s.log.Info("root create requested")
	var req rootRulesUpdate
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	rootID := strings.TrimSpace(req.ID)
	if rootID == "" {
		writeError(w, http.StatusBadRequest, "invalid_root_id", "root id is required")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeError(w, http.StatusBadRequest, "invalid_root_path", "root path is required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, root := range s.cfg.Roots {
		if root.ID == rootID {
			writeError(w, http.StatusConflict, "root_already_exists", "root id already exists")
			return
		}
	}
	root := rootConfigFromUpdate(rootID, req)
	candidate := config.Clone(s.cfg)
	candidate.Roots = append(candidate.Roots, root)
	if err := candidate.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_root", err.Error())
		return
	}
	if err := s.saveConfigLocked(candidate); err != nil {
		s.log.Error("root create config save failed", "root", rootID, "error", err)
		writeError(w, http.StatusInternalServerError, "config_save_failed", err.Error())
		return
	}
	s.log.Info("root create completed", "root", rootID, "path", root.Path, "aliases", len(root.PathAliases))
	writeJSON(w, http.StatusCreated, map[string]any{"status": "created", "root": root})
}

func rootConfigFromUpdate(rootID string, req rootRulesUpdate) config.RootConfig {
	return config.RootConfig{
		ID: rootID, Name: strings.TrimSpace(req.Name), Path: strings.TrimSpace(req.Path), Enabled: req.Enabled,
		Labels: cleanList(req.Labels, false), CredentialRef: strings.TrimSpace(req.CredentialRef),
		IncludeExtensions: cleanExtensions(req.IncludeExtensions), ExcludeExtensions: cleanExtensions(req.ExcludeExtensions),
		IncludeFilePatterns: cleanList(req.IncludeFilePatterns, false), ExcludeFilePatterns: cleanList(req.ExcludeFilePatterns, false),
		IncludeFolderPatterns: cleanList(req.IncludeFolderPatterns, false), ExcludeFolderPatterns: cleanList(req.ExcludeFolderPatterns, false),
		ExcludePatterns:   cleanList(req.ExcludePatterns, false),
		PathAliases:       cleanPathAliases(req.PathAliases),
		ContentExtraction: req.ContentExtraction,
		OCR:               req.OCR,
		Hashing:           req.Hashing,
		CollectOwnership:  req.CollectOwnership,
	}
}

func (s *Server) updateRootRules(w http.ResponseWriter, r *http.Request) {
	rootID := r.PathValue("root_id")
	s.log.Info("root rules update requested", "root", rootID)
	var req rootRulesUpdate
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := config.Clone(s.cfg)
	for i := range candidate.Roots {
		if candidate.Roots[i].ID != rootID {
			continue
		}
		if strings.TrimSpace(req.Path) == "" {
			writeError(w, http.StatusBadRequest, "invalid_root_path", "root path is required")
			return
		}
		previousPath := candidate.Roots[i].Path
		candidate.Roots[i].Path = strings.TrimSpace(req.Path)
		candidate.Roots[i].Name = strings.TrimSpace(req.Name)
		candidate.Roots[i].Enabled = req.Enabled
		candidate.Roots[i].Labels = cleanList(req.Labels, false)
		candidate.Roots[i].IncludeExtensions = cleanExtensions(req.IncludeExtensions)
		candidate.Roots[i].ExcludeExtensions = cleanExtensions(req.ExcludeExtensions)
		candidate.Roots[i].IncludeFilePatterns = cleanList(req.IncludeFilePatterns, false)
		candidate.Roots[i].ExcludeFilePatterns = cleanList(req.ExcludeFilePatterns, false)
		candidate.Roots[i].IncludeFolderPatterns = cleanList(req.IncludeFolderPatterns, false)
		candidate.Roots[i].ExcludeFolderPatterns = cleanList(req.ExcludeFolderPatterns, false)
		candidate.Roots[i].ExcludePatterns = cleanList(req.ExcludePatterns, false)
		candidate.Roots[i].CredentialRef = strings.TrimSpace(req.CredentialRef)
		candidate.Roots[i].PathAliases = preservePreviousRootPath(previousPath, candidate.Roots[i].Path, cleanPathAliases(req.PathAliases))
		candidate.Roots[i].ContentExtraction = req.ContentExtraction
		candidate.Roots[i].OCR = req.OCR
		candidate.Roots[i].Hashing = req.Hashing
		candidate.Roots[i].CollectOwnership = req.CollectOwnership
		if err := candidate.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_root", err.Error())
			return
		}
		if err := s.saveConfigLocked(candidate); err != nil {
			s.log.Error("root rules config save failed", "root", rootID, "error", err)
			writeError(w, http.StatusInternalServerError, "config_save_failed", err.Error())
			return
		}
		repairRequired := !samePathAlias(previousPath, candidate.Roots[i].Path)
		s.log.Info("root rules update completed", "root", rootID, "old_path", previousPath, "new_path", candidate.Roots[i].Path, "aliases", len(candidate.Roots[i].PathAliases), "repair_required", repairRequired)
		writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "root": candidate.Roots[i], "repair_required": repairRequired})
		return
	}
	s.log.Warn("root rules update failed; root not found", "root", rootID)
	writeError(w, http.StatusNotFound, "root_not_found", "root not found")
}

func (s *Server) crawls(w http.ResponseWriter, r *http.Request) {
	runs, err := s.cat.RecentCrawls(r.Context(), 25)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "catalog_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"crawls": runs})
}

func (s *Server) configView(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type redactedAuth struct {
		Mode      string `json:"mode"`
		TokenSet  bool   `json:"token_set"`
		TokenFile string `json:"token_file,omitempty"`
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server":     map[string]any{"bind": s.cfg.Server.Bind, "bind_addresses": s.cfg.Server.BindAddresses, "public_base_url": s.cfg.Server.PublicBaseURL, "auth": redactedAuth{Mode: s.cfg.Server.Auth.Mode, TokenSet: s.searchToken != "", TokenFile: s.cfg.Server.Auth.TokenFile}},
		"management": map[string]any{"enabled": s.cfg.Management.Enabled, "bind": s.cfg.Management.Bind, "bind_addresses": s.cfg.Management.BindAddresses, "auth": redactedAuth{Mode: s.cfg.Management.Auth.Mode, TokenSet: s.adminToken != "", TokenFile: s.cfg.Management.Auth.TokenFile}},
		"index":      s.cfg.Index,
		"crawler":    s.cfg.Crawler,
		"watcher":    s.cfg.Watcher,
		"roots":      s.cfg.Roots,
	})
}

type configBackup struct {
	FormatVersion string               `json:"format_version"`
	ExportedAt    time.Time            `json:"exported_at"`
	Index         config.IndexConfig   `json:"index"`
	Crawler       config.CrawlerConfig `json:"crawler"`
	Watcher       config.WatcherConfig `json:"watcher"`
	Roots         []config.RootConfig  `json:"roots"`
}

func (s *Server) redactedConfigBackup() configBackup {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return configBackup{
		FormatVersion: "1", ExportedAt: time.Now().UTC(), Index: s.cfg.Index,
		Crawler: s.cfg.Crawler, Watcher: s.cfg.Watcher,
		Roots: append([]config.RootConfig(nil), s.cfg.Roots...),
	}
}

func (s *Server) exportConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.redactedConfigBackup())
}

func (s *Server) importConfig(w http.ResponseWriter, r *http.Request) {
	s.log.Info("config import requested")
	var backup configBackup
	if err := decodeJSON(w, r, &backup); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if backup.FormatVersion != "1" {
		writeError(w, http.StatusBadRequest, "unsupported_backup_format", "format_version 1 is required")
		return
	}
	s.mu.Lock()
	candidate := config.Clone(s.cfg)
	candidate.Index = backup.Index
	candidate.Crawler = backup.Crawler
	candidate.Watcher = backup.Watcher
	candidate.Roots = backup.Roots
	if err := candidate.Validate(); err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, "invalid_config_backup", err.Error())
		return
	}
	err := s.saveConfigLocked(candidate)
	s.mu.Unlock()
	if err != nil {
		s.log.Error("config import save failed", "error", err)
		writeError(w, http.StatusInternalServerError, "config_save_failed", err.Error())
		return
	}
	s.log.Info("config import completed", "roots", len(backup.Roots), "restart_recommended", true)
	writeJSON(w, http.StatusOK, map[string]any{"status": "imported", "restart_recommended": true})
}

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	states, err := s.cat.RootStates(r.Context())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "catalog_unavailable", err.Error())
		return
	}
	runs, err := s.cat.RecentCrawls(r.Context(), 50)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "catalog_unavailable", err.Error())
		return
	}
	logs, logErr := s.readLogTail(256, 256*1024)
	logInfo := map[string]any{"available": logErr == nil, "path": s.currentLogPath(), "entries": logs}
	if logErr != nil {
		logInfo["error"] = logErr.Error()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"format_version": "1", "generated_at": time.Now().UTC(),
		"service": map[string]any{"protocol_version": "1.1", "service_instance": s.instanceID, "version": version.Version, "generation": maxGeneration(states)},
		"config":  s.redactedConfigBackup(), "metrics": s.crawler.Stats(),
		"root_states": states, "recent_crawls": runs,
		"logs": logInfo,
	})
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	limit := 300
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		var parsed int
		if _, err := fmt.Sscanf(value, "%d", &parsed); err == nil && parsed > 0 {
			limit = min(parsed, 2000)
		}
	}
	lines, err := s.readLogTail(limit, 512*1024)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "logs_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": s.currentLogPath(), "lines": lines})
}

func (s *Server) currentLogPath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.logPath
}

func (s *Server) readLogTail(limit int, maxBytes int64) ([]string, error) {
	path := s.currentLogPath()
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("log file path is not configured")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	size := info.Size()
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			return nil, err
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if start > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	out := make([]string, 0, min(limit, len(lines)))
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, line)
	}
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	dataDir := config.ResolveDataDir(s.configPath, s.cfg.Index.DataDir)
	s.mu.RUnlock()
	size, err := indexSizeBytes(dataDir)
	if err != nil {
		s.log.Debug("could not read index size", "error", err)
	}
	writeJSON(w, http.StatusOK, struct {
		crawler.Stats
		IndexSizeBytes int64 `json:"index_size_bytes"`
	}{Stats: s.crawler.Stats(), IndexSizeBytes: size})
}

func indexSizeBytes(dataDir string) (int64, error) {
	entries, err := os.ReadDir(dataDir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var size int64
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "qsurfer-search.db") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return 0, err
		}
		size += info.Size()
	}
	return size, nil
}

func (s *Server) pauseCrawler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seconds int `json:"seconds"`
	}
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Seconds <= 0 {
		req.Seconds = 300
	}
	if req.Seconds > 86400 {
		req.Seconds = 86400
	}
	until := s.crawler.Pause(time.Duration(req.Seconds) * time.Second)
	s.log.Info("crawler paused", "seconds", req.Seconds, "until", until)
	writeJSON(w, http.StatusOK, map[string]any{"status": "paused", "paused_until": until, "paused_until_unix": until.Unix()})
}

func (s *Server) resumeCrawler(w http.ResponseWriter, r *http.Request) {
	s.crawler.Resume()
	rootIDs := s.takeResumeRoots()
	s.scheduleRootCrawls(rootIDs, "manual_resume")
	s.log.Info("crawler resumed", "rescheduled_roots", rootIDs)
	writeJSON(w, http.StatusOK, map[string]any{"status": "resumed", "rescheduled_roots": rootIDs})
}

func (s *Server) stopCrawler(w http.ResponseWriter, r *http.Request) {
	until, requestedRoots := s.crawler.CancelAllAndPause(24 * time.Hour)
	s.rememberResumeRoots(requestedRoots)
	waitCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	stopped := s.crawler.WaitAll(waitCtx)
	cancel()
	activeRoots := s.crawler.ActiveRoots()
	if stopped {
		s.log.Info("crawler stop completed", "requested_roots", requestedRoots, "paused_until", until)
		writeJSON(w, http.StatusOK, map[string]any{"status": "stopped", "paused_until": until, "paused_until_unix": until.Unix(), "requested_roots": requestedRoots, "active_roots": activeRoots})
		return
	}
	s.log.Warn("crawler stop still draining", "requested_roots", requestedRoots, "active_roots", activeRoots, "paused_until", until)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "stopping", "paused_until": until, "paused_until_unix": until.Unix(), "requested_roots": requestedRoots, "active_roots": activeRoots})
}

func (s *Server) stopService(w http.ResponseWriter, r *http.Request) {
	s.log.Info("service stop requested")
	activeRoots := s.crawler.CancelAll()
	s.mu.RLock()
	shutdown := s.shutdown
	s.mu.RUnlock()
	if shutdown == nil {
		s.log.Warn("service stop requested but shutdown hook is not configured", "active_roots", activeRoots)
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "stopping", "active_roots": activeRoots, "process_shutdown": false})
		return
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		shutdown()
	}()
	// Do not make the stop request wait for an uninterruptible network read or
	// third-party parser. Run() performs bounded cleanup after this response;
	// returning now lets the process leave promptly even when a share is sick.
	s.log.Info("service stop accepted", "active_roots", activeRoots)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "stopping", "active_roots": activeRoots, "process_shutdown": true})
}

func (s *Server) updateCrawlerSettings(w http.ResponseWriter, r *http.Request) {
	s.log.Info("crawler settings update requested")
	var req struct {
		CollectOwnership  bool                           `json:"collect_ownership"`
		AdaptiveThrottle  config.AdaptiveThrottleConfig  `json:"adaptive_throttle"`
		ContentExtraction config.ContentExtractionConfig `json:"content_extraction"`
		OCR               config.OCRConfig               `json:"ocr"`
		Hashing           config.HashingConfig           `json:"hashing"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := config.Clone(s.cfg)
	candidate.Crawler.CollectOwnership = req.CollectOwnership
	candidate.Crawler.AdaptiveThrottle = req.AdaptiveThrottle
	candidate.Crawler.ContentExtraction = req.ContentExtraction
	candidate.Crawler.OCR = req.OCR
	candidate.Crawler.Hashing = req.Hashing
	candidate.ApplyDefaults()
	if err := candidate.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_crawler_settings", err.Error())
		return
	}
	if err := s.saveConfigLocked(candidate); err != nil {
		s.log.Error("crawler settings config save failed", "error", err)
		writeError(w, http.StatusInternalServerError, "config_save_failed", err.Error())
		return
	}
	s.log.Info("crawler settings update completed", "content_extraction", s.cfg.Crawler.ContentExtraction.Enabled, "ocr", s.cfg.Crawler.OCR.Enabled, "hashing", s.cfg.Crawler.Hashing.Enabled, "ownership", s.cfg.Crawler.CollectOwnership, "adaptive_throttle", s.cfg.Crawler.AdaptiveThrottle.Enabled, "cpu_percent_threshold", s.cfg.Crawler.AdaptiveThrottle.CPUPercentThreshold, "disk_busy_percent_threshold", s.cfg.Crawler.AdaptiveThrottle.DiskBusyPercentThreshold)
	writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "crawler": s.cfg.Crawler})
}

func (s *Server) updateNetworkSettings(w http.ResponseWriter, r *http.Request) {
	s.log.Info("network settings update requested")
	var req struct {
		Server struct {
			Bind          string   `json:"bind"`
			BindAddresses []string `json:"bind_addresses"`
			PublicBaseURL string   `json:"public_base_url"`
		} `json:"server"`
		Management struct {
			Enabled       bool     `json:"enabled"`
			Bind          string   `json:"bind"`
			BindAddresses []string `json:"bind_addresses"`
		} `json:"management"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := config.Clone(s.cfg)
	candidate.Server.Bind = firstBindAddress(req.Server.Bind, req.Server.BindAddresses)
	candidate.Server.BindAddresses = req.Server.BindAddresses
	candidate.Server.PublicBaseURL = strings.TrimSpace(req.Server.PublicBaseURL)
	candidate.Management.Enabled = req.Management.Enabled
	candidate.Management.Bind = firstBindAddress(req.Management.Bind, req.Management.BindAddresses)
	candidate.Management.BindAddresses = req.Management.BindAddresses
	candidate.ApplyDefaults()
	if err := candidate.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_network_settings", err.Error())
		return
	}
	if err := s.saveConfigLocked(candidate); err != nil {
		s.log.Error("network settings config save failed", "error", err)
		writeError(w, http.StatusInternalServerError, "config_save_failed", err.Error())
		return
	}
	s.log.Info("network settings update completed", "server_binds", s.cfg.Server.BindAddresses, "management_binds", s.cfg.Management.BindAddresses, "restart_required", true)
	writeJSON(w, http.StatusOK, map[string]any{
		"status":           "saved",
		"restart_required": true,
		"server":           map[string]any{"bind": s.cfg.Server.Bind, "bind_addresses": s.cfg.Server.BindAddresses, "public_base_url": s.cfg.Server.PublicBaseURL},
		"management":       map[string]any{"enabled": s.cfg.Management.Enabled, "bind": s.cfg.Management.Bind, "bind_addresses": s.cfg.Management.BindAddresses},
	})
}

func (s *Server) configValidate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "valid"})
}

func (s *Server) withAuth(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got != token {
				writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) withAdminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		token := s.adminToken
		s.mu.RUnlock()
		if token == "" {
			writeError(w, http.StatusPreconditionRequired, "admin_setup_required", "set the first admin token in the local management UI")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != token {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next(w, r)
	}
}

func (s *Server) bootstrapAdmin(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		writeError(w, http.StatusForbidden, "bootstrap_local_only", "initial admin setup is only available from localhost")
		return
	}
	var req struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	token := strings.TrimSpace(req.Token)
	if len(token) < 16 {
		writeError(w, http.StatusBadRequest, "weak_admin_token", "admin token must contain at least 16 characters")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adminToken != "" {
		writeError(w, http.StatusConflict, "admin_already_configured", "an admin token is already configured")
		return
	}
	candidate := config.Clone(s.cfg)
	candidate.Management.Auth.Token = token
	candidate.Management.Auth.TokenFile = ""
	if s.searchToken == "" {
		candidate.Server.Auth.Token = token
		candidate.Server.Auth.TokenFile = ""
	}
	if err := s.saveConfigLocked(candidate); err != nil {
		writeError(w, http.StatusInternalServerError, "config_save_failed", err.Error())
		return
	}
	s.adminToken = token
	if s.searchToken == "" {
		s.searchToken = token
	}
	writeJSON(w, http.StatusCreated, map[string]any{"status": "configured"})
}

func (s *Server) withSearchAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		token := s.searchToken
		s.mu.RUnlock()
		if token == "" {
			writeError(w, http.StatusPreconditionRequired, "service_setup_required", "set the first token in the local management UI")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != token {
			writeError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
			return
		}
		next(w, r)
	}
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if value := recover(); value != nil {
				s.log.Error("http request panic recovered", "method", r.Method, "path", r.URL.Path, "panic", value, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "request_panic", "request failed; see service logs")
			}
		}()
		start := time.Now()
		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		level := slog.LevelInfo
		if recorder.status >= 500 {
			level = slog.LevelError
		} else if recorder.status >= 400 {
			level = slog.LevelWarn
		}
		duration := time.Since(start)
		s.mu.RLock()
		accessLog := s.accessLog
		s.mu.RUnlock()
		if accessLog == nil {
			accessLog = s.log
		}
		accessLog.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"bytes", recorder.bytes,
			"duration_ms", duration.Milliseconds(),
			"remote", r.RemoteAddr,
		)
		if recorder.status >= http.StatusBadRequest || duration >= 2*time.Second {
			s.log.Log(r.Context(), level, "http request needs attention",
				"method", r.Method,
				"path", r.URL.Path,
				"status", recorder.status,
				"duration_ms", duration.Milliseconds(),
				"remote", r.RemoteAddr,
			)
		}
	})
}

type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(data []byte) (int, error) {
	n, err := r.ResponseWriter.Write(data)
	r.bytes += n
	return n, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	retryable := status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
	writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": code, "message": message, "retryable": retryable, "unavailable": status == http.StatusServiceUnavailable,
	}})
}

func maxGeneration(states map[string]catalog.RootState) int64 {
	var generation int64
	for _, state := range states {
		if state.LastGeneration > generation {
			generation = state.LastGeneration
		}
	}
	return generation
}

func validatePath(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("path is empty")
	}
	_, err := crawler.ExpandRootPaths(path)
	return err
}

func cleanExtensions(values []string) []string {
	cleaned := cleanList(values, true)
	for i, value := range cleaned {
		cleaned[i] = strings.TrimPrefix(value, ".")
	}
	return cleaned
}

func cleanList(values []string, lower bool) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if lower {
			value = strings.ToLower(value)
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func cleanPathAliases(values []config.PathAlias) []config.PathAlias {
	seen := map[string]bool{}
	aliases := make([]config.PathAlias, 0, len(values))
	for _, value := range values {
		value.ID = strings.TrimSpace(value.ID)
		value.Platform = strings.TrimSpace(value.Platform)
		value.Path = strings.TrimSpace(value.Path)
		value.Target = strings.TrimSpace(value.Target)
		if value.Platform == "" || value.Path == "" {
			continue
		}
		key := strings.ToLower(value.Platform) + "\x00" + strings.ToLower(value.Path) + "\x00" + strings.ToLower(value.Target)
		if seen[key] {
			continue
		}
		seen[key] = true
		aliases = append(aliases, value)
	}
	return aliases
}

func aliasCanonicalTargets(rootPath string, alias config.PathAlias) []string {
	targets := []string{aliasPrimaryCanonicalTarget(rootPath, alias)}
	rootPath = strings.TrimSpace(rootPath)
	if !samePathAlias(targets[0], rootPath) {
		targets = append(targets, rootPath)
	}
	return uniqueScopePaths(targets)
}

func aliasPrimaryCanonicalTarget(rootPath string, alias config.PathAlias) string {
	rootPath = strings.TrimSpace(rootPath)
	if target := strings.TrimSpace(alias.Target); target != "" {
		return target
	}
	if isDriveRootAlias(alias.Path) || isUNCShareRootAlias(alias.Path) {
		return rootPath
	}
	aliasBase := pathLastSegment(alias.Path)
	rootBase := pathLastSegment(rootPath)
	if aliasBase != "" && rootBase != "" && !strings.EqualFold(aliasBase, rootBase) {
		return joinCanonicalScope(rootPath, aliasBase)
	}
	return rootPath
}

func aliasDisplayPriority(alias config.PathAlias) int {
	platform := strings.ToLower(strings.TrimSpace(alias.Platform))
	switch {
	case strings.Contains(platform, "drive"):
		return 300
	case strings.Contains(platform, "unc") || isUNCShareRootAlias(alias.Path):
		return 200
	case strings.Contains(platform, "windows"):
		return 175
	case strings.Contains(platform, "linux") || strings.Contains(platform, "mac"):
		return 100
	default:
		return 0
	}
}

func uniqueScopePaths(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimRight(strings.TrimSpace(value), `\/`)
		if value == "" {
			continue
		}
		key := strings.ToLower(strings.ReplaceAll(value, "\\", "/"))
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func pathLastSegment(value string) string {
	value = strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"), "/")
	if value == "" || (len(value) == 2 && value[1] == ':') {
		return ""
	}
	if strings.HasPrefix(value, "//") && strings.Count(strings.TrimPrefix(value, "//"), "/") == 1 {
		return ""
	}
	if index := strings.LastIndex(value, "/"); index >= 0 {
		return value[index+1:]
	}
	return value
}

func isDriveRootAlias(value string) bool {
	value = strings.TrimRight(strings.TrimSpace(value), `\/`)
	return len(value) == 2 && value[1] == ':'
}

func isUNCShareRootAlias(value string) bool {
	value = strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"), "/")
	if !strings.HasPrefix(value, "//") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(value, "//"), "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != ""
}

func firstBindAddress(fallback string, values []string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return strings.TrimSpace(fallback)
}

func stableAliasID(value string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(value))))
	return fmt.Sprintf("%x", sum[:8])
}

func preservePreviousRootPath(previousPath, currentPath string, aliases []config.PathAlias) []config.PathAlias {
	previousPath = strings.TrimSpace(previousPath)
	currentPath = strings.TrimSpace(currentPath)
	if previousPath == "" || samePathAlias(previousPath, currentPath) || pathAliasExists(previousPath, aliases) {
		return aliases
	}
	alias := config.PathAlias{
		ID:       "previous-" + stableAliasID(previousPath),
		Platform: inferAliasPlatform(previousPath),
		Path:     previousPath,
	}
	return cleanPathAliases(append(aliases, alias))
}

func pathAliasExists(path string, aliases []config.PathAlias) bool {
	for _, alias := range aliases {
		if samePathAlias(path, alias.Path) {
			return true
		}
	}
	return false
}

func samePathAlias(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(strings.TrimSpace(a), `\/`), strings.TrimRight(strings.TrimSpace(b), `\/`))
}

func inferAliasPlatform(path string) string {
	p := strings.TrimSpace(path)
	if len(p) >= 3 && p[1] == ':' && (p[2] == '\\' || p[2] == '/') {
		return "windows-drive"
	}
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return "windows-unc"
	}
	if strings.HasPrefix(p, "/") {
		return "linux"
	}
	return "service"
}

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
	cfg         *config.Config
	configPath  string
	cat         *catalog.Catalog
	crawler     *crawler.Crawler
	log         *slog.Logger
	mu          sync.RWMutex
	searchToken string
	adminToken  string
	instanceID  string
	searchSlots chan struct{}
	shutdown    func()
}

type rootAliasView struct {
	AliasID   string `json:"alias_id"`
	Platform  string `json:"platform"`
	Path      string `json:"path"`
	Target    string `json:"target,omitempty"`
	Canonical bool   `json:"canonical"`
}

func New(cfg *config.Config, configPath string, cat *catalog.Catalog, cr *crawler.Crawler, log *slog.Logger, searchToken, adminToken string) *Server {
	return &Server{cfg: cfg, configPath: configPath, cat: cat, crawler: cr, log: log, searchToken: searchToken, adminToken: adminToken, instanceID: uuid.NewString(), searchSlots: make(chan struct{}, 32)}
}

func (s *Server) SetShutdown(shutdown func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.shutdown = shutdown
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
	mux.HandleFunc("GET /admin/v1/service", s.withAdminAuth(s.health))
	mux.HandleFunc("GET /admin/v1/config", s.withAdminAuth(s.configView))
	mux.HandleFunc("GET /admin/v1/config/export", s.withAdminAuth(s.exportConfig))
	mux.HandleFunc("POST /admin/v1/config/import", s.withAdminAuth(s.importConfig))
	mux.HandleFunc("POST /admin/v1/config/validate", s.withAdminAuth(s.configValidate))
	mux.HandleFunc("GET /admin/v1/metrics", s.withAdminAuth(s.metrics))
	mux.HandleFunc("GET /admin/v1/diagnostics", s.withAdminAuth(s.diagnostics))
	mux.HandleFunc("POST /admin/v1/crawler/pause", s.withAdminAuth(s.pauseCrawler))
	mux.HandleFunc("POST /admin/v1/crawler/resume", s.withAdminAuth(s.resumeCrawler))
	mux.HandleFunc("POST /admin/v1/service/stop", s.withAdminAuth(s.stopService))
	mux.HandleFunc("PUT /admin/v1/crawler/settings", s.withAdminAuth(s.updateCrawlerSettings))
	mux.HandleFunc("PUT /admin/v1/network", s.withAdminAuth(s.updateNetworkSettings))
	mux.HandleFunc("GET /admin/v1/roots", s.withAdminAuth(s.roots))
	mux.HandleFunc("POST /admin/v1/roots", s.withAdminAuth(s.createRoot))
	mux.HandleFunc("DELETE /admin/v1/roots/{root_id}", s.withAdminAuth(s.deleteRoot))
	mux.HandleFunc("POST /admin/v1/roots/{root_id}/crawl", s.withAdminAuth(s.crawlRoot))
	mux.HandleFunc("POST /admin/v1/roots/{root_id}/clear-index", s.withAdminAuth(s.clearRootIndex))
	mux.HandleFunc("POST /admin/v1/roots/{root_id}/repair-index", s.withAdminAuth(s.repairRootIndex))
	mux.HandleFunc("POST /admin/v1/roots/{root_id}/validate", s.withAdminAuth(s.validateRoot))
	mux.HandleFunc("PUT /admin/v1/roots/{root_id}/rules", s.withAdminAuth(s.updateRootRules))
	mux.HandleFunc("GET /admin/v1/crawls", s.withAdminAuth(s.crawls))
	return s.requestLog(mux)
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
	states, _ := s.cat.RootStates(r.Context())
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
	writeJSON(w, http.StatusOK, map[string]any{
		"protocol_version": "1.1",
		"service_instance": s.instanceID,
		"service_version":  version.Version,
		"features": map[string]bool{
			"metadata_search": true,
			"content_search": s.anyRootUses(func(root config.RootConfig) bool {
				return root.ContentExtractionEnabled(s.cfg.Crawler.ContentExtraction.Enabled)
			}),
			"ocr_search": s.anyRootUses(func(root config.RootConfig) bool {
				return root.ContentExtractionEnabled(s.cfg.Crawler.ContentExtraction.Enabled) && root.OCREnabled(s.cfg.Crawler.OCR.Enabled)
			}),
			"acl_filtering":   false,
			"content_hashing": s.anyRootUses(func(root config.RootConfig) bool { return root.HashingEnabled(s.cfg.Crawler.Hashing.Enabled) }),
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
			"max_page_size":           s.cfg.Index.MaxResults,
			"max_path_scopes":         100,
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

func (s *Server) anyRootUses(include func(config.RootConfig) bool) bool {
	for _, root := range s.cfg.Roots {
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
	if !s.acquireSearchSlot(w) {
		return
	}
	defer s.releaseSearchSlot()
	var req catalog.SearchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	if !s.acquireSearchSlot(w) {
		return
	}
	defer s.releaseSearchSlot()
	s.searchWithKind(w, r, "folder", s.cfg.Index.MaxResults)
}

func (s *Server) suggest(w http.ResponseWriter, r *http.Request) {
	if !s.acquireSearchSlot(w) {
		return
	}
	defer s.releaseSearchSlot()
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
}

type scopeResolution struct {
	displayAliases []displayAlias
}

func (r scopeResolution) applyDisplayAliases(resp *catalog.SearchResponse) {
	for i := range resp.Results {
		bestLength := -1
		for _, alias := range r.displayAliases {
			doc := &resp.Results[i]
			if doc.RootID != alias.rootID {
				continue
			}
			suffix, ok := scopedPathSuffix(doc.Path, alias.canonicalPath)
			if !ok || len(alias.canonicalPath) <= bestLength {
				continue
			}
			doc.DisplayPath = joinCanonicalScope(alias.displayPath, suffix)
			bestLength = len(alias.canonicalPath)
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
			resolution.displayAliases = append(resolution.displayAliases, displayAlias{rootID: root.ID, canonicalPath: aliasPrimaryCanonicalTarget(root.Path, alias), displayPath: aliasPath})
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
			resolution.displayAliases = append(resolution.displayAliases, displayAlias{rootID: first.match.rootID, canonicalPath: first.match.path, displayPath: first.display})
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
			if resolvedRoots[root.ID] {
				req.Filters.Roots = append(req.Filters.Roots, root.ID)
			}
		}
	}
	return resolution, nil
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

func (s *Server) clearRootIndex(w http.ResponseWriter, r *http.Request) {
	rootID := r.PathValue("root_id")
	s.log.Info("root clear-index requested", "root", rootID)
	var req struct {
		ConfirmRootID string `json:"confirm_root_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
		s.log.Warn("root index repair failed; root not found", "root", rootID)
		writeError(w, http.StatusNotFound, "root_not_found", "root not found")
		return
	}
	if strings.TrimSpace(root.Path) == "" {
		s.log.Warn("root index repair failed; empty root path", "root", rootID)
		writeError(w, http.StatusBadRequest, "invalid_root_path", "root path is required")
		return
	}
	if !s.stopRootCrawl(r.Context(), rootID) {
		s.log.Warn("root index repair could not stop running crawl", "root", rootID)
		writeError(w, http.StatusConflict, "root_still_stopping", "root crawl is still stopping; retry shortly")
		return
	}
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
		for _, target := range aliasCanonicalTargets(root.Path, alias) {
			s.log.Debug("root index repair checking alias", "root", rootID, "alias", aliasPath, "canonical", target)
			nextRewritten, nextMerged, err := s.cat.RewriteRootPath(r.Context(), rootID, aliasPath, target)
			if err != nil {
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
	embeddedRewritten, embeddedMerged, err := s.cat.RepairEmbeddedRootPath(r.Context(), rootID, root.Path)
	if err != nil {
		s.log.Error("root embedded-path repair failed", "root", rootID, "canonical", root.Path, "error", err)
		writeError(w, http.StatusInternalServerError, "root_path_repair_failed", err.Error())
		return
	}
	rewritten += embeddedRewritten
	merged += embeddedMerged
	s.log.Info("root index repair completed", "root", rootID, "aliases_checked", checkedAliases, "paths_rewritten", rewritten, "duplicate_paths_merged", merged, "embedded_paths_rewritten", embeddedRewritten, "embedded_paths_merged", embeddedMerged)
	writeJSON(w, http.StatusOK, map[string]any{"status": "repaired", "root_id": rootID, "aliases_checked": checkedAliases, "paths_rewritten": rewritten, "duplicate_paths_merged": merged, "aliases_repaired": repairedAliases, "embedded_paths_rewritten": embeddedRewritten, "embedded_paths_merged": embeddedMerged})
}

func (s *Server) deleteRoot(w http.ResponseWriter, r *http.Request) {
	rootID := r.PathValue("root_id")
	s.log.Info("root delete requested", "root", rootID)
	var req struct {
		ConfirmRootID string `json:"confirm_root_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	removed, err := s.cat.DeactivateRoot(r.Context(), rootID)
	if err != nil {
		s.mu.Unlock()
		s.log.Error("root delete failed", "root", rootID, "error", err)
		writeError(w, http.StatusInternalServerError, "catalog_clear_failed", err.Error())
		return
	}
	previous := s.cfg.Roots
	s.cfg.Roots = append([]config.RootConfig(nil), s.cfg.Roots[:index]...)
	s.cfg.Roots = append(s.cfg.Roots, previous[index+1:]...)
	if err := config.Save(s.configPath, s.cfg); err != nil {
		s.cfg.Roots = previous
		s.mu.Unlock()
		s.log.Error("root delete config save failed", "root", rootID, "error", err)
		writeError(w, http.StatusInternalServerError, "config_save_failed", err.Error())
		return
	}
	s.mu.Unlock()
	s.log.Info("root delete completed", "root", rootID, "documents_marked_deleted", removed)
	writeJSON(w, http.StatusOK, map[string]any{"status": "deleted", "root_id": rootID, "documents_removed": removed, "cleanup": "deferred"})
}

func (s *Server) stopRootCrawl(ctx context.Context, rootID string) bool {
	if !s.crawler.IsRootRunning(rootID) {
		return true
	}
	s.crawler.CancelRoot(rootID)
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.crawler.WaitRoot(waitCtx, rootID)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	candidate := *s.cfg
	candidate.Roots = append(candidate.Roots, root)
	if err := candidate.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_root", err.Error())
		return
	}
	s.cfg.Roots = candidate.Roots
	if err := config.Save(s.configPath, s.cfg); err != nil {
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cfg.Roots {
		if s.cfg.Roots[i].ID != rootID {
			continue
		}
		if strings.TrimSpace(req.Path) == "" {
			writeError(w, http.StatusBadRequest, "invalid_root_path", "root path is required")
			return
		}
		previousPath := s.cfg.Roots[i].Path
		s.cfg.Roots[i].Path = strings.TrimSpace(req.Path)
		s.cfg.Roots[i].Name = strings.TrimSpace(req.Name)
		s.cfg.Roots[i].Enabled = req.Enabled
		s.cfg.Roots[i].Labels = cleanList(req.Labels, false)
		s.cfg.Roots[i].IncludeExtensions = cleanExtensions(req.IncludeExtensions)
		s.cfg.Roots[i].ExcludeExtensions = cleanExtensions(req.ExcludeExtensions)
		s.cfg.Roots[i].IncludeFilePatterns = cleanList(req.IncludeFilePatterns, false)
		s.cfg.Roots[i].ExcludeFilePatterns = cleanList(req.ExcludeFilePatterns, false)
		s.cfg.Roots[i].IncludeFolderPatterns = cleanList(req.IncludeFolderPatterns, false)
		s.cfg.Roots[i].ExcludeFolderPatterns = cleanList(req.ExcludeFolderPatterns, false)
		s.cfg.Roots[i].ExcludePatterns = cleanList(req.ExcludePatterns, false)
		s.cfg.Roots[i].CredentialRef = strings.TrimSpace(req.CredentialRef)
		s.cfg.Roots[i].PathAliases = preservePreviousRootPath(previousPath, s.cfg.Roots[i].Path, cleanPathAliases(req.PathAliases))
		s.cfg.Roots[i].ContentExtraction = req.ContentExtraction
		s.cfg.Roots[i].OCR = req.OCR
		s.cfg.Roots[i].Hashing = req.Hashing
		s.cfg.Roots[i].CollectOwnership = req.CollectOwnership
		rewritten, merged, err := s.cat.RewriteRootPath(r.Context(), rootID, previousPath, s.cfg.Roots[i].Path)
		if err != nil {
			s.log.Error("root rules path rewrite failed", "root", rootID, "old_path", previousPath, "new_path", s.cfg.Roots[i].Path, "error", err)
			writeError(w, http.StatusInternalServerError, "root_path_rewrite_failed", err.Error())
			return
		}
		if err := config.Save(s.configPath, s.cfg); err != nil {
			s.log.Error("root rules config save failed", "root", rootID, "error", err)
			writeError(w, http.StatusInternalServerError, "config_save_failed", err.Error())
			return
		}
		s.log.Info("root rules update completed", "root", rootID, "old_path", previousPath, "new_path", s.cfg.Roots[i].Path, "aliases", len(s.cfg.Roots[i].PathAliases), "paths_rewritten", rewritten, "duplicate_paths_merged", merged)
		writeJSON(w, http.StatusOK, map[string]any{"status": "saved", "root": s.cfg.Roots[i], "paths_rewritten": rewritten, "duplicate_paths_merged": merged})
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
	if err := json.NewDecoder(r.Body).Decode(&backup); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if backup.FormatVersion != "1" {
		writeError(w, http.StatusBadRequest, "unsupported_backup_format", "format_version 1 is required")
		return
	}
	s.mu.Lock()
	candidate := *s.cfg
	candidate.Index = backup.Index
	candidate.Crawler = backup.Crawler
	candidate.Watcher = backup.Watcher
	candidate.Roots = backup.Roots
	if err := candidate.Validate(); err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusBadRequest, "invalid_config_backup", err.Error())
		return
	}
	s.cfg.Index = candidate.Index
	s.cfg.Crawler = candidate.Crawler
	s.cfg.Watcher = candidate.Watcher
	s.cfg.Roots = candidate.Roots
	err := config.Save(s.configPath, s.cfg)
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
	writeJSON(w, http.StatusOK, map[string]any{
		"format_version": "1", "generated_at": time.Now().UTC(),
		"service": map[string]any{"protocol_version": "1.1", "service_instance": s.instanceID, "version": version.Version, "generation": maxGeneration(states)},
		"config":  s.redactedConfigBackup(), "metrics": s.crawler.Stats(),
		"root_states": states, "recent_crawls": runs,
		"logs": map[string]any{"available": false, "reason": "no file log sink is configured"},
	})
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
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
	s.log.Info("crawler resumed")
	writeJSON(w, http.StatusOK, map[string]any{"status": "resumed"})
}

func (s *Server) stopService(w http.ResponseWriter, r *http.Request) {
	s.log.Info("service stop requested")
	s.crawler.CancelAll()
	waitCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	stopped := s.crawler.WaitAll(waitCtx)
	cancel()
	s.mu.RLock()
	shutdown := s.shutdown
	s.mu.RUnlock()
	if shutdown == nil {
		s.log.Warn("service stop requested but shutdown hook is not configured", "crawls_stopped", stopped)
		writeJSON(w, http.StatusAccepted, map[string]any{"status": "stopping", "crawls_stopped": stopped, "process_shutdown": false})
		return
	}
	go func() {
		time.Sleep(100 * time.Millisecond)
		shutdown()
	}()
	s.log.Info("service stop accepted", "crawls_stopped", stopped)
	writeJSON(w, http.StatusAccepted, map[string]any{"status": "stopping", "crawls_stopped": stopped, "process_shutdown": true})
}

func (s *Server) updateCrawlerSettings(w http.ResponseWriter, r *http.Request) {
	s.log.Info("crawler settings update requested")
	var req struct {
		CollectOwnership  bool                           `json:"collect_ownership"`
		ContentExtraction config.ContentExtractionConfig `json:"content_extraction"`
		OCR               config.OCRConfig               `json:"ocr"`
		Hashing           config.HashingConfig           `json:"hashing"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := *s.cfg
	candidate.Crawler.CollectOwnership = req.CollectOwnership
	candidate.Crawler.ContentExtraction = req.ContentExtraction
	candidate.Crawler.OCR = req.OCR
	candidate.Crawler.Hashing = req.Hashing
	candidate.ApplyDefaults()
	if err := candidate.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_crawler_settings", err.Error())
		return
	}
	s.cfg.Crawler = candidate.Crawler
	if err := config.Save(s.configPath, s.cfg); err != nil {
		s.log.Error("crawler settings config save failed", "error", err)
		writeError(w, http.StatusInternalServerError, "config_save_failed", err.Error())
		return
	}
	s.log.Info("crawler settings update completed", "content_extraction", s.cfg.Crawler.ContentExtraction.Enabled, "ocr", s.cfg.Crawler.OCR.Enabled, "hashing", s.cfg.Crawler.Hashing.Enabled, "ownership", s.cfg.Crawler.CollectOwnership)
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := *s.cfg
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
	s.cfg.Server.Bind = candidate.Server.Bind
	s.cfg.Server.BindAddresses = candidate.Server.BindAddresses
	s.cfg.Server.PublicBaseURL = candidate.Server.PublicBaseURL
	s.cfg.Management.Enabled = candidate.Management.Enabled
	s.cfg.Management.Bind = candidate.Management.Bind
	s.cfg.Management.BindAddresses = candidate.Management.BindAddresses
	if err := config.Save(s.configPath, s.cfg); err != nil {
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
	s.cfg.Management.Auth.Token = token
	s.cfg.Management.Auth.TokenFile = ""
	if s.searchToken == "" {
		s.cfg.Server.Auth.Token = token
		s.cfg.Server.Auth.TokenFile = ""
	}
	if err := config.Save(s.configPath, s.cfg); err != nil {
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
		start := time.Now()
		recorder := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		level := slog.LevelInfo
		if recorder.status >= 500 {
			level = slog.LevelError
		} else if recorder.status >= 400 {
			level = slog.LevelWarn
		}
		s.log.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"bytes", recorder.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
		)
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
	aliasBase := pathLastSegment(alias.Path)
	rootBase := pathLastSegment(rootPath)
	if aliasBase != "" && rootBase != "" && !strings.EqualFold(aliasBase, rootBase) {
		return joinCanonicalScope(rootPath, aliasBase)
	}
	return rootPath
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

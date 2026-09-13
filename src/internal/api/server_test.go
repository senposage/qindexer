package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"qindexer/internal/catalog"
	"qindexer/internal/config"
	"qindexer/internal/crawler"
)

func TestAdminBootstrapLocksManagementUntilTokenIsSet(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	server := New(cfg, configPath, cat, crawler.New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil))), slog.New(slog.NewTextHandler(io.Discard, nil)), "", "")
	handler := server.AdminHandler()

	before := httptest.NewRequest(http.MethodGet, "/admin/v1/service", nil)
	before.RemoteAddr = "127.0.0.1:12345"
	beforeResult := httptest.NewRecorder()
	handler.ServeHTTP(beforeResult, before)
	if beforeResult.Code != http.StatusPreconditionRequired {
		t.Fatalf("expected setup lock, got %d: %s", beforeResult.Code, beforeResult.Body.String())
	}

	bootstrap := httptest.NewRequest(http.MethodPost, "/admin/v1/bootstrap", bytes.NewBufferString(`{"token":"sixteen-character-token"}`))
	bootstrap.RemoteAddr = "127.0.0.1:12345"
	bootstrapResult := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapResult, bootstrap)
	if bootstrapResult.Code != http.StatusCreated {
		t.Fatalf("expected bootstrap success, got %d: %s", bootstrapResult.Code, bootstrapResult.Body.String())
	}

	after := httptest.NewRequest(http.MethodGet, "/admin/v1/service", nil)
	after.RemoteAddr = "127.0.0.1:12345"
	after.Header.Set("Authorization", "Bearer sixteen-character-token")
	afterResult := httptest.NewRecorder()
	handler.ServeHTTP(afterResult, after)
	if afterResult.Code != http.StatusOK {
		t.Fatalf("expected authenticated service response, got %d: %s", afterResult.Code, afterResult.Body.String())
	}

	search := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	search.Header.Set("Authorization", "Bearer sixteen-character-token")
	searchResult := httptest.NewRecorder()
	server.SearchHandler().ServeHTTP(searchResult, search)
	if searchResult.Code != http.StatusOK {
		t.Fatalf("expected authenticated search response, got %d: %s", searchResult.Code, searchResult.Body.String())
	}

	create := httptest.NewRequest(http.MethodPost, "/admin/v1/roots", bytes.NewBufferString(`{"id":"finance","name":"Finance share","path":"C:\\Finance","enabled":true,"exclude_extensions":["tmp"]}`))
	create.Header.Set("Authorization", "Bearer sixteen-character-token")
	createResult := httptest.NewRecorder()
	handler.ServeHTTP(createResult, create)
	if createResult.Code != http.StatusCreated {
		t.Fatalf("expected root creation, got %d: %s", createResult.Code, createResult.Body.String())
	}
	if len(cfg.Roots) != 1 || cfg.Roots[0].ID != "finance" || cfg.Roots[0].Name != "Finance share" {
		t.Fatalf("unexpected created roots: %#v", cfg.Roots)
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/admin/v1/roots/finance", bytes.NewBufferString(`{"confirm_root_id":"finance"}`))
	deleteReq.Header.Set("Authorization", "Bearer sixteen-character-token")
	deleteResult := httptest.NewRecorder()
	handler.ServeHTTP(deleteResult, deleteReq)
	if deleteResult.Code != http.StatusOK {
		t.Fatalf("expected root deletion success, got %d: %s", deleteResult.Code, deleteResult.Body.String())
	}
	if len(cfg.Roots) != 0 {
		t.Fatalf("root configuration was not removed: %#v", cfg.Roots)
	}
}

func TestMetricsIncludesIndexSize(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "data")
	cat, err := catalog.Open(ctx, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	cfg := &config.Config{Management: config.ManagementConfig{Auth: config.AuthConfig{Mode: "admin-token", Token: "sixteen-character-token"}}}
	server := New(cfg, filepath.Join(t.TempDir(), "config.yaml"), cat, crawler.New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil))), slog.New(slog.NewTextHandler(io.Discard, nil)), "sixteen-character-token", "sixteen-character-token")
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics", nil)
	req.Header.Set("Authorization", "Bearer sixteen-character-token")
	result := httptest.NewRecorder()
	server.AdminHandler().ServeHTTP(result, req)
	if result.Code != http.StatusOK || !bytes.Contains(result.Body.Bytes(), []byte(`"index_size_bytes"`)) {
		t.Fatalf("expected index size metric, got %d: %s", result.Code, result.Body.String())
	}
}

func TestRequestAccessLogIsSeparateFromOperationalLog(t *testing.T) {
	var operational, access bytes.Buffer
	server := &Server{
		log:       slog.New(slog.NewTextHandler(&operational, nil)),
		accessLog: slog.New(slog.NewTextHandler(&access, nil)),
	}
	handler := server.requestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/metrics", nil)
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, request)
	if !strings.Contains(access.String(), "http request") {
		t.Fatalf("expected access log entry, got %q", access.String())
	}
	if operational.Len() != 0 {
		t.Fatalf("routine request should not occupy operational log: %q", operational.String())
	}
}

func TestPublicAdminStatusDoesNotExposeRootDetails(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	cfg := &config.Config{
		Management: config.ManagementConfig{Auth: config.AuthConfig{Mode: "admin-token", Token: "sixteen-character-token"}},
		Roots:      []config.RootConfig{{ID: "private-root", Name: "Private root", Path: `Z:\Private\Client Matters`, Enabled: true}},
	}
	server := New(cfg, filepath.Join(t.TempDir(), "config.yaml"), cat, crawler.New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil))), slog.New(slog.NewTextHandler(io.Discard, nil)), "", "sixteen-character-token")
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/status", nil)
	result := httptest.NewRecorder()
	server.AdminHandler().ServeHTTP(result, request)
	if result.Code != http.StatusOK {
		t.Fatalf("expected public status, got %d: %s", result.Code, result.Body.String())
	}
	body := result.Body.String()
	if !bytes.Contains([]byte(body), []byte(`"admin_configured":true`)) {
		t.Fatalf("expected setup state in public status, got %s", body)
	}
	if bytes.Contains([]byte(body), []byte("private-root")) || bytes.Contains([]byte(body), []byte("Client Matters")) {
		t.Fatalf("public status leaked root data: %s", body)
	}
}

func TestUpdateCrawlerSettingsSavesAdaptiveThrottle(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{Management: config.ManagementConfig{Auth: config.AuthConfig{Mode: "admin-token", Token: "sixteen-character-token"}}}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	server := New(cfg, configPath, cat, crawler.New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil))), slog.New(slog.NewTextHandler(io.Discard, nil)), "", "sixteen-character-token")
	request := httptest.NewRequest(http.MethodPut, "/admin/v1/crawler/settings", bytes.NewBufferString(`{
		"collect_ownership":false,
		"adaptive_throttle":{"enabled":true,"sample_interval_seconds":7,"cpu_percent_threshold":63,"disk_busy_percent_threshold":58,"recovery_samples":5},
		"content_extraction":{"enabled":false},
		"ocr":{"enabled":false},
		"hashing":{"enabled":false}
	}`))
	request.Header.Set("Authorization", "Bearer sixteen-character-token")
	result := httptest.NewRecorder()
	server.AdminHandler().ServeHTTP(result, request)
	if result.Code != http.StatusOK {
		t.Fatalf("expected crawler settings update, got %d: %s", result.Code, result.Body.String())
	}
	throttle := cfg.Crawler.AdaptiveThrottle
	if !throttle.Enabled || throttle.SampleIntervalSeconds != 7 || throttle.CPUPercentThreshold != 63 || throttle.DiskBusyPercentThreshold != 58 || throttle.RecoverySamples != 5 {
		t.Fatalf("adaptive throttle was not saved: %#v", throttle)
	}
}

func TestSearchDoesNotServeStaleUnconfiguredRoots(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	modified := time.Now().UTC()
	for _, doc := range []catalog.Document{
		{ID: "configured-doc", RootID: "current", Path: `/mnt/current/Budget.docx`, NormalizedPath: catalog.NormalizePath(`/mnt/current/Budget.docx`), Name: "Budget.docx", Extension: "docx", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: catalog.Signature(1, modified)},
		{ID: "stale-doc", RootID: "shared", Path: `X:\Budget.docx`, NormalizedPath: catalog.NormalizePath(`X:\Budget.docx`), Name: "Budget.docx", Extension: "docx", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: catalog.Signature(1, modified)},
	} {
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{
		Index:      config.IndexConfig{MaxResults: 20},
		Management: config.ManagementConfig{Auth: config.AuthConfig{Mode: "admin-token", Token: "sixteen-character-token"}},
		Roots:      []config.RootConfig{{ID: "current", Path: `/mnt/current`, Enabled: true}},
	}
	server := New(cfg, filepath.Join(t.TempDir(), "config.yaml"), cat, crawler.New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil))), slog.New(slog.NewTextHandler(io.Discard, nil)), "sixteen-character-token", "sixteen-character-token")
	req := httptest.NewRequest(http.MethodPost, "/v1/search", bytes.NewBufferString(`{"query":"budget","limit":10}`))
	req.Header.Set("Authorization", "Bearer sixteen-character-token")
	result := httptest.NewRecorder()
	server.SearchHandler().ServeHTTP(result, req)
	if result.Code != http.StatusOK {
		t.Fatalf("expected search success, got %d: %s", result.Code, result.Body.String())
	}
	if bytes.Contains(result.Body.Bytes(), []byte(`"root_id":"shared"`)) || bytes.Contains(result.Body.Bytes(), []byte(`X:\\Budget.docx`)) {
		t.Fatalf("stale unconfigured root leaked into search response: %s", result.Body.String())
	}
	if !bytes.Contains(result.Body.Bytes(), []byte(`"root_id":"current"`)) {
		t.Fatalf("configured root missing from search response: %s", result.Body.String())
	}
}

func TestUpdateNetworkSettingsSavesMultipleBindAddresses(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{Management: config.ManagementConfig{Auth: config.AuthConfig{Mode: "admin-token", Token: "sixteen-character-token"}}}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	server := New(cfg, configPath, cat, crawler.New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil))), slog.New(slog.NewTextHandler(io.Discard, nil)), "", "sixteen-character-token")
	req := httptest.NewRequest(http.MethodPut, "/admin/v1/network", bytes.NewBufferString(`{"server":{"bind_addresses":["127.0.0.1:41973","10.44.0.8:41973"],"public_base_url":"http://10.44.0.8:41973"},"management":{"enabled":true,"bind_addresses":["127.0.0.1:41974","192.168.50.9:41974"]}}`))
	req.Header.Set("Authorization", "Bearer sixteen-character-token")
	result := httptest.NewRecorder()
	server.AdminHandler().ServeHTTP(result, req)
	if result.Code != http.StatusOK {
		t.Fatalf("expected network settings save, got %d: %s", result.Code, result.Body.String())
	}
	if cfg.Server.Bind != "127.0.0.1:41973" || len(cfg.Server.BindAddresses) != 2 || cfg.Server.BindAddresses[1] != "10.44.0.8:41973" {
		t.Fatalf("unexpected server binds: %#v", cfg.Server)
	}
	if !cfg.Management.Enabled || cfg.Management.Bind != "127.0.0.1:41974" || len(cfg.Management.BindAddresses) != 2 || cfg.Management.BindAddresses[1] != "192.168.50.9:41974" {
		t.Fatalf("unexpected management binds: %#v", cfg.Management)
	}
	if !bytes.Contains(result.Body.Bytes(), []byte(`"restart_required":true`)) {
		t.Fatalf("expected restart flag, got %s", result.Body.String())
	}
}

func TestRootAliasesUseConfiguredOrDeterministicIDs(t *testing.T) {
	root := config.RootConfig{ID: "finance", Path: `\\nas\finance`, PathAliases: []config.PathAlias{
		{ID: "finance-x", Platform: "windows-drive", Path: `X:\Finance`},
		{Platform: "linux", Path: "/mnt/finance"},
	}}
	aliases := rootAliases(root)
	if len(aliases) != 3 || aliases[0].AliasID != "finance:canonical" || !aliases[0].Canonical || aliases[1].AliasID != "finance-x" {
		t.Fatalf("configured alias ID was not preserved: %#v", aliases)
	}
	if aliases[2].AliasID == "" || aliases[2].AliasID != rootAliases(root)[2].AliasID {
		t.Fatalf("derived alias ID is not stable: %#v", aliases[2])
	}
}

func TestUpdateRootRulesPreservesPreviousRootPathAsAlias(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Management: config.ManagementConfig{Auth: config.AuthConfig{Mode: "admin-token", Token: "sixteen-character-token"}},
		Roots:      []config.RootConfig{{ID: "shared", Path: `X:\`, Enabled: true}},
	}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	server := New(cfg, configPath, cat, crawler.New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil))), slog.New(slog.NewTextHandler(io.Discard, nil)), "", "sixteen-character-token")
	req := httptest.NewRequest(http.MethodPut, "/admin/v1/roots/shared/rules", bytes.NewBufferString(`{"path":"/mnt/shared-somenumbershre","enabled":true,"path_aliases":[{"id":"linux-cifs","platform":"linux","path":"/mnt/shared-somenumbershre"}]}`))
	req.Header.Set("Authorization", "Bearer sixteen-character-token")
	result := httptest.NewRecorder()
	server.AdminHandler().ServeHTTP(result, req)
	if result.Code != http.StatusOK {
		t.Fatalf("expected update success, got %d: %s", result.Code, result.Body.String())
	}
	if cfg.Roots[0].Path != "/mnt/shared-somenumbershre" {
		t.Fatalf("root path was not updated: %#v", cfg.Roots[0])
	}
	if len(cfg.Roots[0].PathAliases) != 2 {
		t.Fatalf("expected linux alias plus preserved windows alias: %#v", cfg.Roots[0].PathAliases)
	}
	if cfg.Roots[0].PathAliases[1].Platform != "windows-drive" || cfg.Roots[0].PathAliases[1].Path != `X:\` {
		t.Fatalf("previous Windows root path was not preserved: %#v", cfg.Roots[0].PathAliases)
	}
}

func TestUpdateRootRulesSavesPathWithoutBlockingOnRepair(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Management: config.ManagementConfig{Auth: config.AuthConfig{Mode: "admin-token", Token: "sixteen-character-token"}},
		Roots:      []config.RootConfig{{ID: "shared", Path: `X:\`, Enabled: true}},
	}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	modified := time.Now().UTC()
	doc := catalog.Document{ID: "doc", RootID: "shared", Path: `X:\Cases\Budget.docx`, NormalizedPath: catalog.NormalizePath(`X:\Cases\Budget.docx`), Name: "Budget.docx", Extension: "docx", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: catalog.Signature(1, modified)}
	if _, err := cat.UpsertDocument(ctx, doc); err != nil {
		t.Fatal(err)
	}
	server := New(cfg, configPath, cat, crawler.New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil))), slog.New(slog.NewTextHandler(io.Discard, nil)), "", "sixteen-character-token")
	req := httptest.NewRequest(http.MethodPut, "/admin/v1/roots/shared/rules", bytes.NewBufferString(`{"path":"/mnt/shared-real","enabled":true}`))
	req.Header.Set("Authorization", "Bearer sixteen-character-token")
	result := httptest.NewRecorder()
	server.AdminHandler().ServeHTTP(result, req)
	if result.Code != http.StatusOK {
		t.Fatalf("expected update success, got %d: %s", result.Code, result.Body.String())
	}
	if !bytes.Contains(result.Body.Bytes(), []byte(`"repair_required":true`)) {
		t.Fatalf("expected repair required in response, got %s", result.Body.String())
	}
	if cfg.Roots[0].Path != `/mnt/shared-real` {
		t.Fatalf("root path was not saved: %#v", cfg.Roots[0])
	}
	if len(cfg.Roots[0].PathAliases) != 1 || !strings.HasPrefix(cfg.Roots[0].PathAliases[0].ID, "previous-") || cfg.Roots[0].PathAliases[0].Path != `X:\` {
		t.Fatalf("previous root path alias was not preserved: %#v", cfg.Roots[0].PathAliases)
	}
	resp, err := cat.Search(ctx, catalog.SearchRequest{Filters: catalog.SearchFilters{Roots: []string{"shared"}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Path != `X:\Cases\Budget.docx` {
		t.Fatalf("plain save should not rewrite indexed paths: %#v", resp.Results)
	}
}

func TestRepairRootIndexRewritesAliasPaths(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Management: config.ManagementConfig{Auth: config.AuthConfig{Mode: "admin-token", Token: "sixteen-character-token"}},
		Roots: []config.RootConfig{{
			ID:      "shared",
			Path:    `/mnt/shared-real`,
			Enabled: true,
			PathAliases: []config.PathAlias{
				{ID: "previous-old-x", Platform: "windows-drive", Path: `X:\`},
				{ID: "shared-x", Platform: "windows-drive", Path: `Y:\`},
			},
		}},
	}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	modified := time.Now().UTC()
	for _, item := range []struct {
		id   string
		path string
	}{
		{"old", `X:\Cases\Budget.docx`},
		{"new", `/mnt/shared-real/Cases/Budget.docx`},
	} {
		doc := catalog.Document{ID: item.id, RootID: "shared", Path: item.path, NormalizedPath: catalog.NormalizePath(item.path), Name: filepath.Base(item.path), Extension: "docx", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: catalog.Signature(1, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	server := New(cfg, configPath, cat, crawler.New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil))), slog.New(slog.NewTextHandler(io.Discard, nil)), "", "sixteen-character-token")
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/roots/shared/repair-index", bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer sixteen-character-token")
	result := httptest.NewRecorder()
	server.AdminHandler().ServeHTTP(result, req)
	if result.Code != http.StatusOK {
		t.Fatalf("expected repair success, got %d: %s", result.Code, result.Body.String())
	}
	if !bytes.Contains(result.Body.Bytes(), []byte(`"duplicate_paths_merged":1`)) {
		t.Fatalf("expected duplicate merge in response, got %s", result.Body.String())
	}
	if !bytes.Contains(result.Body.Bytes(), []byte(`"previous_aliases_removed":1`)) {
		t.Fatalf("expected previous alias cleanup in response, got %s", result.Body.String())
	}
	if len(cfg.Roots[0].PathAliases) != 1 || cfg.Roots[0].PathAliases[0].ID != "shared-x" {
		t.Fatalf("previous alias was not removed or stable alias was not kept: %#v", cfg.Roots[0].PathAliases)
	}
	resp, err := cat.Search(ctx, catalog.SearchRequest{Query: "budget", Filters: catalog.SearchFilters{Roots: []string{"shared"}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Path != `/mnt/shared-real/Cases/Budget.docx` {
		t.Fatalf("repair left duplicate or wrong path: %#v", resp.Results)
	}
}

func TestRepairRootIndexMergesSubfolderAliasUnderBroadMountRoot(t *testing.T) {
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &config.Config{
		Management: config.ManagementConfig{Auth: config.AuthConfig{Mode: "admin-token", Token: "sixteen-character-token"}},
		Roots: []config.RootConfig{{
			ID:      "legal",
			Path:    `/srv/qindexer/mounts/team-share`,
			Enabled: true,
			PathAliases: []config.PathAlias{
				{ID: "old-x", Platform: "windows-drive", Path: `X:\Legal`},
			},
		}},
	}
	if err := config.Save(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	modified := time.Now().UTC()
	oldPath := `X:\Legal\Matters\Example\brief.docx`
	newPath := `/srv/qindexer/mounts/team-share/Legal/Matters/Example/brief.docx`
	for _, item := range []struct {
		id   string
		path string
	}{
		{"old", oldPath},
		{"new", newPath},
	} {
		doc := catalog.Document{ID: item.id, RootID: "legal", Path: item.path, NormalizedPath: catalog.NormalizePath(item.path), Name: filepath.Base(item.path), Extension: "docx", Size: 1, ModifiedAt: modified, LastSeenGeneration: 1, Signature: catalog.Signature(1, modified)}
		if _, err := cat.UpsertDocument(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	server := New(cfg, configPath, cat, crawler.New(cfg, cat, slog.New(slog.NewTextHandler(io.Discard, nil))), slog.New(slog.NewTextHandler(io.Discard, nil)), "", "sixteen-character-token")
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/roots/legal/repair-index", bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer sixteen-character-token")
	result := httptest.NewRecorder()
	server.AdminHandler().ServeHTTP(result, req)
	if result.Code != http.StatusOK {
		t.Fatalf("expected repair success, got %d: %s", result.Code, result.Body.String())
	}
	if !bytes.Contains(result.Body.Bytes(), []byte(`"duplicate_paths_merged":1`)) {
		t.Fatalf("expected duplicate merge in response, got %s", result.Body.String())
	}
	resp, err := cat.Search(ctx, catalog.SearchRequest{Query: "brief", Filters: catalog.SearchFilters{Roots: []string{"legal"}}, Limit: 10}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Results) != 1 || resp.Results[0].Path != newPath {
		t.Fatalf("subfolder alias repair left duplicate or wrong path: %#v", resp.Results)
	}
}

func TestResolveScopeAliasesInfersSubfolderAliasTarget(t *testing.T) {
	cfg := &config.Config{Roots: []config.RootConfig{{
		ID:   "legal",
		Path: `/srv/qindexer/mounts/team-share`,
		PathAliases: []config.PathAlias{
			{Platform: "windows-drive", Path: `X:\Legal`},
		},
	}}}
	server := &Server{cfg: cfg}
	req := catalog.SearchRequest{Filters: catalog.SearchFilters{IncludePaths: []string{`X:\Legal\Matters`}}}
	if _, err := server.resolveScopeAliases(&req); err != nil {
		t.Fatal(err)
	}
	want := `/srv/qindexer/mounts/team-share/Legal/Matters`
	if req.Filters.IncludePaths[0] != want {
		t.Fatalf("scope alias resolved to %q, want %q", req.Filters.IncludePaths[0], want)
	}
	resp := catalog.SearchResponse{Results: []catalog.Document{{
		RootID: "legal",
		Path:   `/srv/qindexer/mounts/team-share/Legal/Matters/example.docx`,
	}}}
	resolution, err := server.resolveScopeAliases(&catalog.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resolution.applyDisplayAliases(&resp)
	wantDisplay := `X:\Legal\Matters\example.docx`
	if resp.Results[0].DisplayPath != wantDisplay {
		t.Fatalf("display path = %q, want %q", resp.Results[0].DisplayPath, wantDisplay)
	}
}

func TestResolveScopeAliasesMapsShareRootAliasesToRoot(t *testing.T) {
	cfg := &config.Config{Roots: []config.RootConfig{{
		ID:   "shared",
		Path: `/mnt/shared`,
		PathAliases: []config.PathAlias{
			{Platform: "windows-drive", Path: `Q:\`},
			{Platform: "windows-unc", Path: `\\files01\shared`},
		},
	}}}
	server := &Server{cfg: cfg}
	req := catalog.SearchRequest{Filters: catalog.SearchFilters{IncludePaths: []string{`Q:\Cases\Budget.docx`}}}
	if _, err := server.resolveScopeAliases(&req); err != nil {
		t.Fatal(err)
	}
	if got, want := req.Filters.IncludePaths[0], `/mnt/shared/Cases/Budget.docx`; got != want {
		t.Fatalf("drive root alias resolved to %q, want %q", got, want)
	}
	req = catalog.SearchRequest{Filters: catalog.SearchFilters{IncludePaths: []string{`\\files01\shared\Cases\Budget.docx`}}}
	if _, err := server.resolveScopeAliases(&req); err != nil {
		t.Fatal(err)
	}
	if got, want := req.Filters.IncludePaths[0], `/mnt/shared/Cases/Budget.docx`; got != want {
		t.Fatalf("UNC share root alias resolved to %q, want %q", got, want)
	}
}

func TestDisplayAliasesPreferDriveAliasOverUNCAtSameTarget(t *testing.T) {
	cfg := &config.Config{Roots: []config.RootConfig{{
		ID:   "shared",
		Path: `/mnt/shared`,
		PathAliases: []config.PathAlias{
			{Platform: "windows-unc", Path: `\\files01\shared`},
			{Platform: "windows-drive", Path: `Q:\`},
		},
	}}}
	server := &Server{cfg: cfg}
	resolution, err := server.resolveScopeAliases(&catalog.SearchRequest{})
	if err != nil {
		t.Fatal(err)
	}
	resp := catalog.SearchResponse{Results: []catalog.Document{{
		RootID: "shared",
		Path:   `/mnt/shared/Cases/Budget.docx`,
	}}}
	resolution.applyDisplayAliases(&resp)
	if got, want := resp.Results[0].DisplayPath, `Q:\Cases\Budget.docx`; got != want {
		t.Fatalf("display path = %q, want %q", got, want)
	}
}

func TestResolveScopeAliasesOnlyChangesExplicitScopes(t *testing.T) {
	cfg := &config.Config{Roots: []config.RootConfig{{ID: "finance", Path: `\\nas\finance`, Enabled: true, PathAliases: []config.PathAlias{{Platform: "windows-drive", Path: `X:\Finance`}}}}}
	server := &Server{cfg: cfg}
	req := catalog.SearchRequest{Query: `X:\Finance budget`, Filters: catalog.SearchFilters{IncludePaths: []string{`X:\Finance\Q3`}}}
	if _, err := server.resolveScopeAliases(&req); err != nil {
		t.Fatal(err)
	}
	if req.Query != `X:\Finance budget` {
		t.Fatalf("raw query was rewritten: %q", req.Query)
	}
	if got := req.Filters.IncludePaths[0]; got != `\\nas\finance\Q3` {
		t.Fatalf("unexpected canonical scope %q", got)
	}
	if len(req.Filters.Roots) != 1 || req.Filters.Roots[0] != "finance" {
		t.Fatalf("root was not inferred: %#v", req.Filters.Roots)
	}
}

func TestResolveScopeAliasesRejectsAmbiguity(t *testing.T) {
	cfg := &config.Config{Roots: []config.RootConfig{{ID: "one", Path: `C:\One`, PathAliases: []config.PathAlias{{Platform: "windows-drive", Path: `X:\Shared`}}}, {ID: "two", Path: `C:\Two`, PathAliases: []config.PathAlias{{Platform: "windows-drive", Path: `X:\Shared`}}}}}
	server := &Server{cfg: cfg}
	req := catalog.SearchRequest{Filters: catalog.SearchFilters{IncludePaths: []string{`X:\Shared\docs`}}}
	if _, err := server.resolveScopeAliases(&req); err == nil {
		t.Fatal("expected ambiguous alias error")
	}
}

func TestResolveScopeAliasesUsesEphemeralAliasAndPreservesQuery(t *testing.T) {
	cfg := &config.Config{Roots: []config.RootConfig{{ID: "shared", Path: `\\nas\Shared`}}}
	server := &Server{cfg: cfg}
	req := catalog.SearchRequest{
		Query: `name:"budget" path:X:\Cases`,
		Filters: catalog.SearchFilters{
			IncludePaths: []string{`X:\Cases\Active`},
			ScopeAliases: []catalog.ScopeAlias{{Path: `X:\`, Target: `\\nas\Shared`, Platform: "windows-drive"}},
		},
	}
	resolution, err := server.resolveScopeAliases(&req)
	if err != nil {
		t.Fatalf("expected request alias to resolve: %v", err)
	}
	if req.Query != `name:"budget" path:X:\Cases` {
		t.Fatalf("raw query was rewritten: %q", req.Query)
	}
	if got := req.Filters.IncludePaths[0]; got != `\\nas\Shared\Cases\Active` {
		t.Fatalf("unexpected canonical scope %q", got)
	}
	response := catalog.SearchResponse{Results: []catalog.Document{{RootID: "shared", Path: `\\nas\Shared\Cases\Active\budget.docx`}}}
	resolution.applyDisplayAliases(&response)
	if got := response.Results[0].DisplayPath; got != `X:\Cases\Active\budget.docx` {
		t.Fatalf("unexpected display path %q", got)
	}
}

func TestResolveScopeAliasesRejectsConflictingEphemeralAliases(t *testing.T) {
	cfg := &config.Config{Roots: []config.RootConfig{{ID: "one", Path: `\\nas\One`}, {ID: "two", Path: `\\nas\Two`}}}
	server := &Server{cfg: cfg}
	req := catalog.SearchRequest{Filters: catalog.SearchFilters{
		IncludePaths: []string{`X:\Cases`},
		ScopeAliases: []catalog.ScopeAlias{{Path: `X:\`, Target: `\\nas\One`}, {Path: `X:\`, Target: `\\nas\Two`}},
	}}
	if _, err := server.resolveScopeAliases(&req); err == nil {
		t.Fatal("expected conflicting request aliases to fail")
	}
}

func TestScopedPathSuffixPreservesLinuxCaseAndWindowsCaseFolding(t *testing.T) {
	if _, ok := scopedPathSuffix(`/mnt/Shared/Cases`, `/mnt/shared`); ok {
		t.Fatal("linux path comparison must be case sensitive")
	}
	if _, ok := scopedPathSuffix(`X:\CASES`, `x:\cases`); !ok {
		t.Fatal("windows path comparison must be case insensitive")
	}
}

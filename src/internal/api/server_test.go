package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

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
}

func TestRootAliasesUseConfiguredOrDeterministicIDs(t *testing.T) {
	root := config.RootConfig{ID: "finance", Path: `\\nas\finance`, PathAliases: []config.PathAlias{
		{ID: "finance-x", Platform: "windows-drive", Path: `X:\Finance`},
		{Platform: "linux", Path: "/mnt/finance"},
	}}
	aliases := rootAliases(root)
	if len(aliases) != 2 || aliases[0].AliasID != "finance-x" {
		t.Fatalf("configured alias ID was not preserved: %#v", aliases)
	}
	if aliases[1].AliasID == "" || aliases[1].AliasID != rootAliases(root)[1].AliasID {
		t.Fatalf("derived alias ID is not stable: %#v", aliases[1])
	}
}

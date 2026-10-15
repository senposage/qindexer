package config

import "testing"

func TestRootEnrichmentOverridesGlobalDefaults(t *testing.T) {
	trueValue, falseValue := true, false
	root := RootConfig{ContentExtraction: &trueValue, OCR: &falseValue, Hashing: &falseValue, CollectOwnership: &trueValue}
	if !root.ContentExtractionEnabled(false) || root.OCREnabled(true) || root.HashingEnabled(true) || !root.OwnershipEnabled(false) {
		t.Fatalf("unexpected root overrides: %#v", root)
	}
	if !(RootConfig{}).ContentExtractionEnabled(true) || (RootConfig{}).HashingEnabled(false) {
		t.Fatal("unset root settings should inherit global defaults")
	}
}

func TestBindAddressesDefaultFromLegacyBind(t *testing.T) {
	cfg := Config{Server: ServerConfig{Bind: "127.0.0.1:41973"}, Management: ManagementConfig{Bind: "127.0.0.1:41974"}}
	cfg.ApplyDefaults()
	if len(cfg.Server.BindAddresses) != 1 || cfg.Server.BindAddresses[0] != "127.0.0.1:41973" {
		t.Fatalf("server bind addresses did not inherit legacy bind: %#v", cfg.Server.BindAddresses)
	}
	if len(cfg.Management.BindAddresses) != 1 || cfg.Management.BindAddresses[0] != "127.0.0.1:41974" {
		t.Fatalf("management bind addresses did not inherit legacy bind: %#v", cfg.Management.BindAddresses)
	}
}

func TestBindAddressesValidateHostPort(t *testing.T) {
	cfg := Config{Server: ServerConfig{BindAddresses: []string{"127.0.0.1"}}, Management: ManagementConfig{BindAddresses: []string{"127.0.0.1:41974"}}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected bind address without port to fail")
	}
}

func TestAdaptiveThrottleValidation(t *testing.T) {
	cfg := Config{
		Server:     ServerConfig{Bind: "127.0.0.1:41973"},
		Management: ManagementConfig{Bind: "127.0.0.1:41974"},
		Crawler: CrawlerConfig{AdaptiveThrottle: AdaptiveThrottleConfig{
			Enabled: true, SampleIntervalSeconds: 5, CPUPercentThreshold: 101, DiskBusyPercentThreshold: 70, RecoverySamples: 3,
		}},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid CPU threshold to fail validation")
	}
}

func TestEmptyRootFirstRunConfigurationIsValid(t *testing.T) {
	cfg := Config{
		Server:     ServerConfig{Bind: "127.0.0.1:41973", Auth: AuthConfig{Mode: "static-token"}},
		Management: ManagementConfig{Enabled: true, Bind: "127.0.0.1:41974", Auth: AuthConfig{Mode: "admin-token"}},
		Index:      IndexConfig{DataDir: "data", Database: DatabaseConfig{Driver: "sqlite"}},
		Roots:      []RootConfig{},
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("empty first-run configuration should validate: %v", err)
	}
}

func TestDatabaseDefaultsToPostgresAndRequiresDSN(t *testing.T) {
	cfg := Config{Server: ServerConfig{Bind: "127.0.0.1:41973"}, Management: ManagementConfig{Bind: "127.0.0.1:41974"}}
	cfg.ApplyDefaults()
	if cfg.Index.Database.Driver != "postgres" {
		t.Fatalf("database driver = %q, want postgres", cfg.Index.Database.Driver)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected default postgres configuration without a DSN to fail validation")
	}
	cfg.Index.Database.DSN = "postgres://qindexer:secret@127.0.0.1:5432/qindexer?sslmode=disable"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected postgres configuration with a DSN to validate: %v", err)
	}
}

func TestCrawlerDefaultsScaleWithAvailableCPUs(t *testing.T) {
	cfg := Config{}
	cfg.ApplyDefaults()
	if cfg.Crawler.RootParallelism < 1 || cfg.Crawler.DirectoryWorkerCount < 16 || cfg.Crawler.MetadataWorkerCount < 16 || cfg.Crawler.MetadataQueueSize < 10000 || cfg.Crawler.IndexBatchSize < 2000 {
		t.Fatalf("unexpected crawler defaults: %#v", cfg.Crawler)
	}
	if len(cfg.Crawler.ContentExtraction.ExcludeExtensions) == 0 || len(cfg.Crawler.OCR.ExcludeExtensions) == 0 {
		t.Fatalf("enrichment exclusions were not populated: %#v", cfg.Crawler)
	}
}

func TestEnrichmentExclusionsNormalizeAsLineEntries(t *testing.T) {
	cfg := Config{Crawler: CrawlerConfig{
		ContentExtraction: ContentExtractionConfig{ExcludeExtensions: []string{" JPG ", ".png", "jpg"}},
		OCR:               OCRConfig{ExcludeExtensions: []string{".PDF", " pdf "}},
	}}
	cfg.ApplyDefaults()
	if got := cfg.Crawler.ContentExtraction.ExcludeExtensions; len(got) != 2 || got[0] != "jpg" || got[1] != "png" {
		t.Fatalf("content exclusions = %#v", got)
	}
	if got := cfg.Crawler.OCR.ExcludeExtensions; len(got) != 1 || got[0] != "pdf" {
		t.Fatalf("OCR exclusions = %#v", got)
	}
}

func TestLegacyCrawlerDefaultProfileUpgradesToHostSizedValues(t *testing.T) {
	baseline := Config{}
	baseline.ApplyDefaults()
	cfg := Config{Crawler: CrawlerConfig{
		RootParallelism:      1,
		DirectoryWorkerCount: 16,
		MetadataWorkerCount:  32,
		MetadataQueueSize:    5000,
		IndexBatchSize:       1000,
	}}
	cfg.ApplyDefaults()
	if cfg.Crawler.RootParallelism != baseline.Crawler.RootParallelism || cfg.Crawler.DirectoryWorkerCount != baseline.Crawler.DirectoryWorkerCount || cfg.Crawler.MetadataWorkerCount != baseline.Crawler.MetadataWorkerCount || cfg.Crawler.MetadataQueueSize != baseline.Crawler.MetadataQueueSize || cfg.Crawler.IndexBatchSize != baseline.Crawler.IndexBatchSize {
		t.Fatalf("legacy profile = %#v, want host defaults %#v", cfg.Crawler, baseline.Crawler)
	}
}

func TestCustomizedCrawlerProfileIsNotOverwritten(t *testing.T) {
	cfg := Config{Crawler: CrawlerConfig{
		RootParallelism:      1,
		DirectoryWorkerCount: 12,
		MetadataWorkerCount:  24,
		MetadataQueueSize:    4000,
		IndexBatchSize:       800,
	}}
	cfg.ApplyDefaults()
	if cfg.Crawler.DirectoryWorkerCount != 12 || cfg.Crawler.MetadataWorkerCount != 24 || cfg.Crawler.MetadataQueueSize != 4000 || cfg.Crawler.IndexBatchSize != 800 {
		t.Fatalf("custom profile was overwritten: %#v", cfg.Crawler)
	}
}

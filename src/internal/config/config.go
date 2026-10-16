package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server     ServerConfig     `yaml:"server" json:"server"`
	Management ManagementConfig `yaml:"management" json:"management"`
	Index      IndexConfig      `yaml:"index" json:"index"`
	Crawler    CrawlerConfig    `yaml:"crawler" json:"crawler"`
	Watcher    WatcherConfig    `yaml:"watcher" json:"watcher"`
	Roots      []RootConfig     `yaml:"roots" json:"roots"`
}

type ServerConfig struct {
	Bind          string     `yaml:"bind" json:"bind"`
	BindAddresses []string   `yaml:"bind_addresses" json:"bind_addresses"`
	PublicBaseURL string     `yaml:"public_base_url" json:"public_base_url"`
	Auth          AuthConfig `yaml:"auth" json:"auth"`
}

type ManagementConfig struct {
	Enabled       bool       `yaml:"enabled" json:"enabled"`
	Bind          string     `yaml:"bind" json:"bind"`
	BindAddresses []string   `yaml:"bind_addresses" json:"bind_addresses"`
	Auth          AuthConfig `yaml:"auth" json:"auth"`
}

type AuthConfig struct {
	Mode      string `yaml:"mode" json:"mode"`
	Token     string `yaml:"token" json:"token,omitempty"`
	TokenFile string `yaml:"token_file" json:"token_file,omitempty"`
}

type IndexConfig struct {
	DataDir               string         `yaml:"data_dir" json:"data_dir"`
	Database              DatabaseConfig `yaml:"database" json:"database"`
	CommitIntervalSeconds int            `yaml:"commit_interval_seconds" json:"commit_interval_seconds"`
	MaxResults            int            `yaml:"max_results" json:"max_results"`
}

// DatabaseConfig selects the catalog backend. PostgreSQL is the default for
// durable multi-root indexes; SQLite remains available for portable installs.
type DatabaseConfig struct {
	Driver       string `yaml:"driver" json:"driver"`
	DSN          string `yaml:"dsn" json:"dsn,omitempty"`
	MaxOpenConns int    `yaml:"max_open_conns" json:"max_open_conns"`
	MaxIdleConns int    `yaml:"max_idle_conns" json:"max_idle_conns"`
}

type CrawlerConfig struct {
	ScanIntervalSeconds          int                     `yaml:"scan_interval_seconds" json:"scan_interval_seconds"`
	RootParallelism              int                     `yaml:"root_parallelism" json:"root_parallelism"`
	DirectoryWorkerCount         int                     `yaml:"directory_worker_count" json:"directory_worker_count"`
	MetadataWorkerCount          int                     `yaml:"metadata_worker_count" json:"metadata_worker_count"`
	MetadataQueueSize            int                     `yaml:"metadata_queue_size" json:"metadata_queue_size"`
	IndexBatchSize               int                     `yaml:"index_batch_size" json:"index_batch_size"`
	ExcludeFilePatterns          []string                `yaml:"exclude_file_patterns" json:"exclude_file_patterns"`
	ExcludeFolderPatterns        []string                `yaml:"exclude_folder_patterns" json:"exclude_folder_patterns"`
	IgnoreHidden                 bool                    `yaml:"ignore_hidden" json:"ignore_hidden"`
	FollowSymlinks               bool                    `yaml:"follow_symlinks" json:"follow_symlinks"`
	CollectOwnership             bool                    `yaml:"collect_ownership" json:"collect_ownership"`
	MissingAfterSuccessfulCrawls int                     `yaml:"missing_after_successful_crawls" json:"missing_after_successful_crawls"`
	PauseWindows                 []PauseWindow           `yaml:"pause_windows" json:"pause_windows"`
	AdaptiveThrottle             AdaptiveThrottleConfig  `yaml:"adaptive_throttle" json:"adaptive_throttle"`
	ContentExtraction            ContentExtractionConfig `yaml:"content_extraction" json:"content_extraction"`
	OCR                          OCRConfig               `yaml:"ocr" json:"ocr"`
	Hashing                      HashingConfig           `yaml:"hashing" json:"hashing"`
}

type ContentExtractionConfig struct {
	Enabled           bool     `yaml:"enabled" json:"enabled"`
	WorkerCount       int      `yaml:"worker_count" json:"worker_count"`
	QueueSize         int      `yaml:"queue_size" json:"queue_size"`
	MaxFileSizeMB     int64    `yaml:"max_file_size_mb" json:"max_file_size_mb"`
	MaxStoredTextKB   int64    `yaml:"max_stored_text_kb" json:"max_stored_text_kb"`
	ExcludeExtensions []string `yaml:"exclude_extensions" json:"exclude_extensions"`
}

// OCRConfig describes an optional local OCR sidecar. Commands stay configurable
// so a QIndexer build can run unchanged on Windows, Linux, and NAS hosts.
type OCRConfig struct {
	Enabled           bool     `yaml:"enabled" json:"enabled"`
	Engine            string   `yaml:"engine" json:"engine"`
	TesseractCommand  string   `yaml:"tesseract_command" json:"tesseract_command"`
	OCRmyPDFCommand   string   `yaml:"ocrmypdf_command" json:"ocrmypdf_command"`
	Languages         string   `yaml:"languages" json:"languages"`
	WorkerCount       int      `yaml:"worker_count" json:"worker_count"`
	QueueSize         int      `yaml:"queue_size" json:"queue_size"`
	MaxFileSizeMB     int64    `yaml:"max_file_size_mb" json:"max_file_size_mb"`
	TimeoutSeconds    int      `yaml:"timeout_seconds" json:"timeout_seconds"`
	ExcludeExtensions []string `yaml:"exclude_extensions" json:"exclude_extensions"`
}

var defaultContentExcludeExtensions = []string{".png", ".jpg", ".jpeg", ".tif", ".tiff", ".bmp", ".gif", ".webp", ".tmp", ".bak", ".part", ".partial", ".crdownload", ".download", ".lnk", ".url", ".ds_store", ".thumbs.db"}
var defaultOCRExcludeExtensions = []string{".tmp", ".bak", ".part", ".partial", ".crdownload", ".download", ".lnk", ".url", ".ds_store", ".thumbs.db"}
var defaultExcludeFilePatterns = []string{"~$*", "Thumbs.db", "ehthumbs.db", "desktop.ini", ".DS_Store", "._*", "*.tmp", "*.temp", "*.bak", "*.partial", "*.part", "*.crdownload", "*.download", "*.swp", "*.swo"}
var defaultExcludeFolderPatterns = []string{"**/.git/**", "**/node_modules/**", "**/.Trash-*/**", "**/@eaDir/**", "**/.AppleDouble/**", "**/.Spotlight-V100/**", "**/@Recently-Snapshot/**", "**/@Recycle/**", "**/#recycle/**", "**/$RECYCLE.BIN/**", "**/RECYCLER/**", "**/.sync/**", "**/.qsync/**", "**/.qsync_sn/**", "**/AppData/Local/Temp/**"}

type HashingConfig struct {
	Enabled       bool  `yaml:"enabled" json:"enabled"`
	WorkerCount   int   `yaml:"worker_count" json:"worker_count"`
	QueueSize     int   `yaml:"queue_size" json:"queue_size"`
	MaxFileSizeMB int64 `yaml:"max_file_size_mb" json:"max_file_size_mb"`
}

type AdaptiveThrottleConfig struct {
	Enabled                  bool    `yaml:"enabled" json:"enabled"`
	SampleIntervalSeconds    int     `yaml:"sample_interval_seconds" json:"sample_interval_seconds"`
	CPUPercentThreshold      float64 `yaml:"cpu_percent_threshold" json:"cpu_percent_threshold"`
	DiskBusyPercentThreshold float64 `yaml:"disk_busy_percent_threshold" json:"disk_busy_percent_threshold"`
	PressureSamples          int     `yaml:"pressure_samples" json:"pressure_samples"`
	RecoverySamples          int     `yaml:"recovery_samples" json:"recovery_samples"`
}

type PauseWindow struct {
	Start string   `yaml:"start" json:"start"`
	End   string   `yaml:"end" json:"end"`
	Days  []string `yaml:"days" json:"days"`
}

type WatcherConfig struct {
	Enabled               bool `yaml:"enabled" json:"enabled"`
	DebounceMilliseconds  int  `yaml:"debounce_milliseconds" json:"debounce_milliseconds"`
	HintCooldownSeconds   int  `yaml:"hint_cooldown_seconds" json:"hint_cooldown_seconds"`
	MaxWatchedDirectories int  `yaml:"max_watched_directories" json:"max_watched_directories"`
	MaxDirtyPathsPerFlush int  `yaml:"max_dirty_paths_per_flush" json:"max_dirty_paths_per_flush"`
	ActivityHalfLifeDays  int  `yaml:"activity_half_life_days" json:"activity_half_life_days"`
	RebalanceMinutes      int  `yaml:"rebalance_minutes" json:"rebalance_minutes"`
}

type RootConfig struct {
	ID                    string      `yaml:"id" json:"id"`
	Name                  string      `yaml:"name" json:"name,omitempty"`
	Path                  string      `yaml:"path" json:"path"`
	Enabled               bool        `yaml:"enabled" json:"enabled"`
	Labels                []string    `yaml:"labels" json:"labels"`
	IncludeExtensions     []string    `yaml:"include_extensions" json:"include_extensions"`
	ExcludeExtensions     []string    `yaml:"exclude_extensions" json:"exclude_extensions"`
	IncludeFilePatterns   []string    `yaml:"include_file_patterns" json:"include_file_patterns"`
	ExcludeFilePatterns   []string    `yaml:"exclude_file_patterns" json:"exclude_file_patterns"`
	IncludeFolderPatterns []string    `yaml:"include_folder_patterns" json:"include_folder_patterns"`
	ExcludeFolderPatterns []string    `yaml:"exclude_folder_patterns" json:"exclude_folder_patterns"`
	ExcludePatterns       []string    `yaml:"exclude_patterns" json:"exclude_patterns"`
	CredentialRef         string      `yaml:"credential_ref" json:"credential_ref"`
	PathAliases           []PathAlias `yaml:"path_aliases" json:"path_aliases,omitempty"`
	ContentExtraction     *bool       `yaml:"content_extraction" json:"content_extraction,omitempty"`
	OCR                   *bool       `yaml:"ocr" json:"ocr,omitempty"`
	// Nil inherits the global crawler exclusion list. An explicit empty list
	// deliberately permits every otherwise supported extension for this root.
	ContentExcludeExtensions *[]string `yaml:"content_exclude_extensions,omitempty" json:"content_exclude_extensions,omitempty"`
	OCRExcludeExtensions     *[]string `yaml:"ocr_exclude_extensions,omitempty" json:"ocr_exclude_extensions,omitempty"`
	Hashing                  *bool     `yaml:"hashing" json:"hashing,omitempty"`
	CollectOwnership         *bool     `yaml:"collect_ownership" json:"collect_ownership,omitempty"`
}

type PathAlias struct {
	ID       string `yaml:"id" json:"id,omitempty"`
	Platform string `yaml:"platform" json:"platform"`
	Path     string `yaml:"path" json:"path"`
	Target   string `yaml:"target,omitempty" json:"target,omitempty"`
}

func (r RootConfig) FriendlyName() string {
	if strings.TrimSpace(r.Name) != "" {
		return r.Name
	}
	return r.ID
}

func (r RootConfig) ContentExtractionEnabled(defaultValue bool) bool {
	return enabledOrDefault(r.ContentExtraction, defaultValue)
}

func (r RootConfig) OCREnabled(defaultValue bool) bool {
	return enabledOrDefault(r.OCR, defaultValue)
}

func (r RootConfig) ContentExcludedExtensions(defaults []string) []string {
	if r.ContentExcludeExtensions == nil {
		return defaults
	}
	return *r.ContentExcludeExtensions
}

func (r RootConfig) OCRExcludedExtensions(defaults []string) []string {
	if r.OCRExcludeExtensions == nil {
		return defaults
	}
	return *r.OCRExcludeExtensions
}

func (r RootConfig) HashingEnabled(defaultValue bool) bool {
	return enabledOrDefault(r.Hashing, defaultValue)
}

func (r RootConfig) OwnershipEnabled(defaultValue bool) bool {
	return enabledOrDefault(r.CollectOwnership, defaultValue)
}

func enabledOrDefault(value *bool, defaultValue bool) bool {
	return value == nil && defaultValue || value != nil && *value
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func ResolveDataDir(configPath string, dataDir string) string {
	if dataDir == "" {
		dataDir = "data"
	}
	if filepath.IsAbs(dataDir) {
		return dataDir
	}
	return filepath.Join(filepath.Dir(configPath), dataDir)
}

func (c *Config) applyDefaults() {
	logicalCPUs := max(1, runtime.GOMAXPROCS(0))
	defaultRootParallelism := min(4, max(1, (logicalCPUs+5)/6))
	defaultDirectoryWorkers := min(64, max(16, logicalCPUs*3))
	defaultMetadataWorkers := min(64, max(16, logicalCPUs*2))
	defaultMetadataQueue := min(100000, max(10000, defaultMetadataWorkers*512))
	defaultIndexBatch := min(8192, max(2000, defaultMetadataWorkers*128))
	legacyCrawlerDefaults := c.Crawler.RootParallelism == 1 &&
		c.Crawler.DirectoryWorkerCount == 16 &&
		c.Crawler.MetadataWorkerCount == 32 &&
		c.Crawler.MetadataQueueSize == 5000 &&
		c.Crawler.IndexBatchSize == 1000
	if c.Server.Bind == "" {
		c.Server.Bind = "127.0.0.1:41973"
	}
	c.Server.BindAddresses = normalizeBindAddresses(c.Server.Bind, c.Server.BindAddresses)
	if c.Management.Bind == "" {
		c.Management.Bind = "127.0.0.1:41974"
	}
	c.Management.BindAddresses = normalizeBindAddresses(c.Management.Bind, c.Management.BindAddresses)
	if c.Index.DataDir == "" {
		c.Index.DataDir = "data"
	}
	if c.Index.Database.Driver == "" {
		c.Index.Database.Driver = "postgres"
	}
	if c.Index.Database.MaxOpenConns <= 0 {
		if strings.EqualFold(c.Index.Database.Driver, "sqlite") {
			c.Index.Database.MaxOpenConns = 8
		} else {
			c.Index.Database.MaxOpenConns = 32
		}
	}
	if c.Index.Database.MaxIdleConns <= 0 {
		if strings.EqualFold(c.Index.Database.Driver, "sqlite") {
			c.Index.Database.MaxIdleConns = 4
		} else {
			c.Index.Database.MaxIdleConns = 8
		}
	}
	if c.Index.CommitIntervalSeconds <= 0 {
		c.Index.CommitIntervalSeconds = 5
	}
	if c.Index.MaxResults <= 0 {
		c.Index.MaxResults = 200
	}
	if c.Crawler.ScanIntervalSeconds <= 0 {
		c.Crawler.ScanIntervalSeconds = 21600
	}
	if legacyCrawlerDefaults {
		// Upgrade the exact historical shipped profile, while preserving every
		// intentionally customized setting already stored by an administrator.
		c.Crawler.RootParallelism = defaultRootParallelism
		c.Crawler.DirectoryWorkerCount = defaultDirectoryWorkers
		c.Crawler.MetadataWorkerCount = defaultMetadataWorkers
		c.Crawler.MetadataQueueSize = defaultMetadataQueue
		c.Crawler.IndexBatchSize = defaultIndexBatch
	} else if c.Crawler.RootParallelism <= 0 {
		c.Crawler.RootParallelism = defaultRootParallelism
	}
	if c.Crawler.DirectoryWorkerCount <= 0 {
		c.Crawler.DirectoryWorkerCount = defaultDirectoryWorkers
	}
	if c.Crawler.MetadataWorkerCount <= 0 {
		c.Crawler.MetadataWorkerCount = defaultMetadataWorkers
	}
	if c.Crawler.MetadataQueueSize <= 0 {
		c.Crawler.MetadataQueueSize = defaultMetadataQueue
	}
	if c.Crawler.IndexBatchSize <= 0 {
		c.Crawler.IndexBatchSize = defaultIndexBatch
	}
	if c.Crawler.ExcludeFilePatterns == nil {
		c.Crawler.ExcludeFilePatterns = append([]string(nil), defaultExcludeFilePatterns...)
	}
	if c.Crawler.ExcludeFolderPatterns == nil {
		c.Crawler.ExcludeFolderPatterns = append([]string(nil), defaultExcludeFolderPatterns...)
	}
	c.Crawler.ExcludeFilePatterns = normalizePatterns(c.Crawler.ExcludeFilePatterns)
	c.Crawler.ExcludeFolderPatterns = normalizePatterns(c.Crawler.ExcludeFolderPatterns)
	if c.Crawler.MissingAfterSuccessfulCrawls <= 0 {
		c.Crawler.MissingAfterSuccessfulCrawls = 3
	}
	if c.Crawler.ContentExtraction.WorkerCount <= 0 {
		c.Crawler.ContentExtraction.WorkerCount = 3
	}
	if c.Crawler.ContentExtraction.QueueSize <= 0 {
		c.Crawler.ContentExtraction.QueueSize = 1000
	}
	if c.Crawler.ContentExtraction.MaxFileSizeMB <= 0 {
		c.Crawler.ContentExtraction.MaxFileSizeMB = 64
	}
	if c.Crawler.ContentExtraction.MaxStoredTextKB <= 0 {
		c.Crawler.ContentExtraction.MaxStoredTextKB = 1024
	}
	if c.Crawler.ContentExtraction.ExcludeExtensions == nil {
		c.Crawler.ContentExtraction.ExcludeExtensions = append([]string(nil), defaultContentExcludeExtensions...)
	}
	c.Crawler.ContentExtraction.ExcludeExtensions = normalizeExtensions(c.Crawler.ContentExtraction.ExcludeExtensions)
	if c.Crawler.OCR.Engine == "" {
		c.Crawler.OCR.Engine = "auto"
	}
	if c.Crawler.OCR.TesseractCommand == "" {
		c.Crawler.OCR.TesseractCommand = "tesseract"
	}
	if c.Crawler.OCR.OCRmyPDFCommand == "" {
		c.Crawler.OCR.OCRmyPDFCommand = "ocrmypdf"
	}
	if c.Crawler.OCR.Languages == "" {
		c.Crawler.OCR.Languages = "eng"
	}
	if c.Crawler.OCR.WorkerCount <= 0 {
		c.Crawler.OCR.WorkerCount = 2
	}
	if c.Crawler.OCR.QueueSize <= 0 {
		c.Crawler.OCR.QueueSize = 100
	}
	if c.Crawler.OCR.MaxFileSizeMB <= 0 {
		c.Crawler.OCR.MaxFileSizeMB = 128
	}
	if c.Crawler.OCR.TimeoutSeconds <= 0 {
		c.Crawler.OCR.TimeoutSeconds = 180
	}
	if c.Crawler.OCR.ExcludeExtensions == nil {
		c.Crawler.OCR.ExcludeExtensions = append([]string(nil), defaultOCRExcludeExtensions...)
	}
	c.Crawler.OCR.ExcludeExtensions = normalizeExtensions(c.Crawler.OCR.ExcludeExtensions)
	for i := range c.Roots {
		c.Roots[i].IncludeExtensions = normalizeExtensions(c.Roots[i].IncludeExtensions)
		c.Roots[i].ExcludeExtensions = normalizeExtensions(c.Roots[i].ExcludeExtensions)
		if c.Roots[i].ContentExcludeExtensions != nil {
			values := normalizeExtensions(*c.Roots[i].ContentExcludeExtensions)
			c.Roots[i].ContentExcludeExtensions = &values
		}
		if c.Roots[i].OCRExcludeExtensions != nil {
			values := normalizeExtensions(*c.Roots[i].OCRExcludeExtensions)
			c.Roots[i].OCRExcludeExtensions = &values
		}
	}
	if c.Crawler.Hashing.WorkerCount <= 0 {
		c.Crawler.Hashing.WorkerCount = 3
	}
	if c.Crawler.Hashing.QueueSize <= 0 {
		c.Crawler.Hashing.QueueSize = 1000
	}
	if c.Crawler.Hashing.MaxFileSizeMB <= 0 {
		c.Crawler.Hashing.MaxFileSizeMB = 2048
	}
	if c.Crawler.AdaptiveThrottle.SampleIntervalSeconds <= 0 {
		c.Crawler.AdaptiveThrottle.SampleIntervalSeconds = 5
	}
	if c.Crawler.AdaptiveThrottle.CPUPercentThreshold <= 0 {
		c.Crawler.AdaptiveThrottle.CPUPercentThreshold = 80
	}
	if c.Crawler.AdaptiveThrottle.DiskBusyPercentThreshold <= 0 {
		c.Crawler.AdaptiveThrottle.DiskBusyPercentThreshold = 70
	}
	if c.Crawler.AdaptiveThrottle.PressureSamples <= 0 {
		c.Crawler.AdaptiveThrottle.PressureSamples = 3
	}
	if c.Crawler.AdaptiveThrottle.RecoverySamples <= 0 {
		c.Crawler.AdaptiveThrottle.RecoverySamples = 3
	}
	if c.Watcher.DebounceMilliseconds <= 0 {
		c.Watcher.DebounceMilliseconds = 1500
	}
	if c.Watcher.HintCooldownSeconds <= 0 {
		c.Watcher.HintCooldownSeconds = 900
	}
	if c.Watcher.MaxWatchedDirectories <= 0 {
		c.Watcher.MaxWatchedDirectories = 5000
	}
	if c.Watcher.MaxDirtyPathsPerFlush <= 0 {
		c.Watcher.MaxDirtyPathsPerFlush = 500
	}
	if c.Watcher.ActivityHalfLifeDays <= 0 {
		c.Watcher.ActivityHalfLifeDays = 60
	}
	if c.Watcher.RebalanceMinutes <= 0 {
		c.Watcher.RebalanceMinutes = 15
	}
}

// ApplyDefaults normalizes configuration submitted through the management API.
func (c *Config) ApplyDefaults() { c.applyDefaults() }

func (c *Config) Validate() error {
	driver := strings.ToLower(strings.TrimSpace(c.Index.Database.Driver))
	if driver != "sqlite" && driver != "postgres" && driver != "postgresql" {
		return fmt.Errorf("index database driver must be postgres or sqlite")
	}
	if (driver == "postgres" || driver == "postgresql") && strings.TrimSpace(c.Index.Database.DSN) == "" {
		return errors.New("index database dsn is required for postgres")
	}
	if c.Index.Database.MaxOpenConns < 1 || c.Index.Database.MaxOpenConns > 256 {
		return fmt.Errorf("index database max_open_conns must be between 1 and 256")
	}
	if c.Index.Database.MaxIdleConns < 0 || c.Index.Database.MaxIdleConns > c.Index.Database.MaxOpenConns {
		return fmt.Errorf("index database max_idle_conns must be between 0 and max_open_conns")
	}
	if err := validateBindAddresses("server", c.Server.BindAddresses); err != nil {
		return err
	}
	if err := validateBindAddresses("management", c.Management.BindAddresses); err != nil {
		return err
	}
	engine := strings.ToLower(strings.TrimSpace(c.Crawler.OCR.Engine))
	if engine != "" && engine != "auto" && engine != "tesseract" && engine != "ocrmypdf" {
		return fmt.Errorf("invalid OCR engine %q; expected auto, tesseract, or ocrmypdf", c.Crawler.OCR.Engine)
	}
	for _, workers := range []struct {
		name  string
		count int
	}{
		{"directory worker limit", c.Crawler.DirectoryWorkerCount},
		{"metadata worker limit", c.Crawler.MetadataWorkerCount},
		{"content worker count", c.Crawler.ContentExtraction.WorkerCount},
		{"OCR worker count", c.Crawler.OCR.WorkerCount},
		{"hash worker count", c.Crawler.Hashing.WorkerCount},
	} {
		if workers.count < 1 || workers.count > 256 {
			return fmt.Errorf("%s must be between 1 and 256", workers.name)
		}
	}
	if c.Crawler.RootParallelism < 1 || c.Crawler.RootParallelism > 32 {
		return fmt.Errorf("root crawl limit must be between 1 and 32")
	}
	if c.Crawler.ScanIntervalSeconds < 60 || c.Crawler.ScanIntervalSeconds > 30*24*60*60 {
		return fmt.Errorf("full reconciliation interval must be between 60 seconds and 30 days")
	}
	if c.Crawler.MetadataQueueSize < 100 || c.Crawler.MetadataQueueSize > 1000000 {
		return fmt.Errorf("metadata queue size must be between 100 and 1000000")
	}
	if c.Crawler.IndexBatchSize < 1 || c.Crawler.IndexBatchSize > 1000000 {
		return fmt.Errorf("index batch size must be between 1 and 1000000")
	}
	throttle := c.Crawler.AdaptiveThrottle
	if throttle.SampleIntervalSeconds < 1 || throttle.SampleIntervalSeconds > 3600 {
		return fmt.Errorf("adaptive throttle sample interval must be between 1 and 3600 seconds")
	}
	if throttle.CPUPercentThreshold <= 0 || throttle.CPUPercentThreshold > 100 {
		return fmt.Errorf("adaptive throttle CPU threshold must be greater than 0 and at most 100")
	}
	if throttle.DiskBusyPercentThreshold <= 0 || throttle.DiskBusyPercentThreshold > 100 {
		return fmt.Errorf("adaptive throttle disk-busy threshold must be greater than 0 and at most 100")
	}
	if throttle.PressureSamples < 1 || throttle.PressureSamples > 3600 {
		return fmt.Errorf("adaptive throttle pressure samples must be between 1 and 3600")
	}
	if throttle.RecoverySamples < 1 || throttle.RecoverySamples > 3600 {
		return fmt.Errorf("adaptive throttle recovery samples must be between 1 and 3600")
	}
	if c.Crawler.ContentExtraction.MaxStoredTextKB < 64 || c.Crawler.ContentExtraction.MaxStoredTextKB > 1024 {
		return fmt.Errorf("stored text limit must be between 64 and 1024 KB")
	}
	if c.Watcher.ActivityHalfLifeDays < 1 || c.Watcher.ActivityHalfLifeDays > 3650 {
		return fmt.Errorf("watcher activity half-life must be between 1 and 3650 days")
	}
	if c.Watcher.HintCooldownSeconds < 1 || c.Watcher.HintCooldownSeconds > 3600 {
		return fmt.Errorf("watcher hint cooldown must be between 1 and 3600 seconds")
	}
	if c.Watcher.MaxWatchedDirectories < 1 || c.Watcher.MaxWatchedDirectories > 250000 {
		return fmt.Errorf("watcher directory limit must be between 1 and 250000")
	}
	if c.Watcher.RebalanceMinutes < 1 || c.Watcher.RebalanceMinutes > 1440 {
		return fmt.Errorf("watcher rebalance interval must be between 1 and 1440 minutes")
	}
	ids := map[string]bool{}
	for _, root := range c.Roots {
		if strings.TrimSpace(root.ID) == "" {
			return errors.New("root id is required")
		}
		if ids[root.ID] {
			return fmt.Errorf("duplicate root id %q", root.ID)
		}
		ids[root.ID] = true
		if strings.TrimSpace(root.Path) == "" {
			return fmt.Errorf("root %q path is required", root.ID)
		}
		for _, alias := range root.PathAliases {
			if strings.TrimSpace(alias.Platform) == "" || strings.TrimSpace(alias.Path) == "" {
				return fmt.Errorf("root %q path aliases require platform and path", root.ID)
			}
		}
	}
	for _, window := range c.Crawler.PauseWindows {
		if _, err := time.Parse("15:04", window.Start); err != nil {
			return fmt.Errorf("invalid crawl pause window start %q", window.Start)
		}
		if _, err := time.Parse("15:04", window.End); err != nil {
			return fmt.Errorf("invalid crawl pause window end %q", window.End)
		}
	}
	return nil
}

func normalizeBindAddresses(primary string, values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, value)
	}
	add(primary)
	for _, value := range values {
		add(value)
	}
	return out
}

func validateBindAddresses(name string, values []string) error {
	if len(values) == 0 {
		return fmt.Errorf("%s bind_addresses must contain at least one address", name)
	}
	for _, value := range values {
		host, port, err := net.SplitHostPort(value)
		if err != nil {
			return fmt.Errorf("%s bind address %q must be host:port", name, value)
		}
		if port == "" {
			return fmt.Errorf("%s bind address %q must include a port", name, value)
		}
		if strings.TrimSpace(host) == "" {
			return fmt.Errorf("%s bind address %q must include a host; use 0.0.0.0 for all IPv4 interfaces", name, value)
		}
	}
	return nil
}

func Save(path string, cfg *Config) error {
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".qindexer-config-*")
	if err != nil {
		return err
	}
	temporaryPath := file.Name()
	defer os.Remove(temporaryPath)
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(b); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0600)
}

// Clone returns an independent configuration snapshot for long-running work.
func Clone(source *Config) *Config {
	if source == nil {
		return &Config{}
	}
	copyConfig := *source
	copyConfig.Server.BindAddresses = append([]string(nil), source.Server.BindAddresses...)
	copyConfig.Management.BindAddresses = append([]string(nil), source.Management.BindAddresses...)
	copyConfig.Crawler.PauseWindows = make([]PauseWindow, len(source.Crawler.PauseWindows))
	for i, window := range source.Crawler.PauseWindows {
		copyConfig.Crawler.PauseWindows[i] = PauseWindow{Start: window.Start, End: window.End, Days: append([]string(nil), window.Days...)}
	}
	copyConfig.Crawler.ContentExtraction.ExcludeExtensions = append([]string(nil), source.Crawler.ContentExtraction.ExcludeExtensions...)
	copyConfig.Crawler.OCR.ExcludeExtensions = append([]string(nil), source.Crawler.OCR.ExcludeExtensions...)
	copyConfig.Crawler.ExcludeFilePatterns = append([]string(nil), source.Crawler.ExcludeFilePatterns...)
	copyConfig.Crawler.ExcludeFolderPatterns = append([]string(nil), source.Crawler.ExcludeFolderPatterns...)
	copyConfig.Roots = make([]RootConfig, len(source.Roots))
	for i, root := range source.Roots {
		copyConfig.Roots[i] = root
		copyConfig.Roots[i].Labels = append([]string(nil), root.Labels...)
		copyConfig.Roots[i].IncludeExtensions = append([]string(nil), root.IncludeExtensions...)
		copyConfig.Roots[i].ExcludeExtensions = append([]string(nil), root.ExcludeExtensions...)
		copyConfig.Roots[i].IncludeFilePatterns = append([]string(nil), root.IncludeFilePatterns...)
		copyConfig.Roots[i].ExcludeFilePatterns = append([]string(nil), root.ExcludeFilePatterns...)
		copyConfig.Roots[i].IncludeFolderPatterns = append([]string(nil), root.IncludeFolderPatterns...)
		copyConfig.Roots[i].ExcludeFolderPatterns = append([]string(nil), root.ExcludeFolderPatterns...)
		copyConfig.Roots[i].ExcludePatterns = append([]string(nil), root.ExcludePatterns...)
		copyConfig.Roots[i].PathAliases = append([]PathAlias(nil), root.PathAliases...)
		copyConfig.Roots[i].ContentExcludeExtensions = cloneStringSlicePointer(root.ContentExcludeExtensions)
		copyConfig.Roots[i].OCRExcludeExtensions = cloneStringSlicePointer(root.OCRExcludeExtensions)
	}
	return &copyConfig
}

func cloneStringSlicePointer(source *[]string) *[]string {
	if source == nil {
		return nil
	}
	values := make([]string, len(*source))
	copy(values, *source)
	return &values
}

func normalizeExtensions(values []string) []string {
	if len(values) == 0 {
		return values
	}
	seen := make(map[string]bool, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "*" {
			if !seen[value] {
				seen[value] = true
				normalized = append(normalized, value)
			}
			continue
		}
		value = strings.TrimLeft(value, ".")
		if value == "" {
			continue
		}
		value = "." + value
		if seen[value] {
			continue
		}
		seen[value] = true
		normalized = append(normalized, value)
	}
	return normalized
}

func normalizePatterns(values []string) []string {
	if len(values) == 0 {
		return values
	}
	seen := make(map[string]bool, len(values))
	cleaned := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		cleaned = append(cleaned, value)
	}
	return cleaned
}

func (a AuthConfig) ResolveToken(baseDir string) (string, error) {
	if a.Token != "" {
		return a.Token, nil
	}
	if a.TokenFile == "" {
		return "", nil
	}
	path := a.TokenFile
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func (c CrawlerConfig) ScanInterval() time.Duration {
	return time.Duration(c.ScanIntervalSeconds) * time.Second
}

func (a AdaptiveThrottleConfig) SampleInterval() time.Duration {
	return time.Duration(a.SampleIntervalSeconds) * time.Second
}

func (w WatcherConfig) Debounce() time.Duration {
	return time.Duration(w.DebounceMilliseconds) * time.Millisecond
}

func (w WatcherConfig) HintCooldown() time.Duration {
	return time.Duration(w.HintCooldownSeconds) * time.Second
}

func (w WatcherConfig) ActivityHalfLife() time.Duration {
	return time.Duration(w.ActivityHalfLifeDays) * 24 * time.Hour
}

func (w WatcherConfig) RebalanceInterval() time.Duration {
	return time.Duration(w.RebalanceMinutes) * time.Minute
}

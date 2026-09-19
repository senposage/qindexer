package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"qindexer/internal/catalog"
	"qindexer/internal/service"
	"qindexer/internal/version"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("command failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	if len(os.Args) < 2 {
		usage()
		return nil
	}
	switch os.Args[1] {
	case "run":
		fs := flag.NewFlagSet("run", flag.ExitOnError)
		configPath := fs.String("config", "configs/example.yaml", "path to config file")
		_ = fs.Parse(os.Args[2:])
		return service.App{ConfigPath: *configPath}.Run(service.SignalContext())
	case "crawl":
		fs := flag.NewFlagSet("crawl", flag.ExitOnError)
		configPath := fs.String("config", "configs/example.yaml", "path to config file")
		rootID := fs.String("root", "", "root id to crawl; empty crawls all enabled roots")
		_ = fs.Parse(os.Args[2:])
		return service.App{ConfigPath: *configPath}.CrawlOnce(service.SignalContext(), *rootID)
	case "status":
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		configPath := fs.String("config", "configs/example.yaml", "path to config file")
		_ = fs.Parse(os.Args[2:])
		return service.App{ConfigPath: *configPath, Logger: log}.Status(service.SignalContext())
	case "db-check":
		fs := flag.NewFlagSet("db-check", flag.ExitOnError)
		databasePath := fs.String("database", "", "SQLite database file to validate read-only")
		_ = fs.Parse(os.Args[2:])
		if *databasePath == "" {
			return fmt.Errorf("--database is required")
		}
		if err := catalog.CheckDatabase(service.SignalContext(), *databasePath); err != nil {
			return err
		}
		fmt.Printf("database integrity check passed: %s\n", *databasePath)
		return nil
	case "db-compact":
		fs := flag.NewFlagSet("db-compact", flag.ExitOnError)
		databasePath := fs.String("database", "", "SQLite database file to compact")
		maxTextKB := fs.Int64("max-stored-text-kb", 1024, "maximum stored text per document in KiB")
		_ = fs.Parse(os.Args[2:])
		if *databasePath == "" {
			return fmt.Errorf("--database is required")
		}
		if filepath.Base(*databasePath) != "qsurfer-search.db" {
			return fmt.Errorf("--database must name qsurfer-search.db")
		}
		if *maxTextKB < 64 || *maxTextKB > 1024 {
			return fmt.Errorf("--max-stored-text-kb must be between 64 and 1024")
		}
		cat, err := catalog.Open(service.SignalContext(), filepath.Dir(*databasePath))
		if err != nil {
			return err
		}
		defer cat.Close()
		result, err := cat.Compact(service.SignalContext(), *maxTextKB*1024)
		if err != nil {
			return err
		}
		fmt.Printf("database compacted: %d -> %d bytes; trimmed %d documents (%d bytes)\n", result.Before.DatabaseBytes, result.After.DatabaseBytes, result.TrimmedDocuments, result.TrimmedTextBytes)
		return nil
	case "version":
		fmt.Printf("qindexer %s (%s)\n", version.Version, version.Commit)
		return nil
	default:
		usage()
		return nil
	}
}

func usage() {
	fmt.Println(`qindexer

Usage:
  qindexer run --config configs/example.yaml
	  qindexer crawl --config configs/example.yaml [--root root-id]
	  qindexer status --config configs/example.yaml
	  qindexer db-check --database path/to/qsurfer-search.db
	  qindexer db-compact --database path/to/qsurfer-search.db [--max-stored-text-kb 1024]
	  qindexer version`)
}

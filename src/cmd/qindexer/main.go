package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"

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
		return service.App{ConfigPath: *configPath, Logger: log}.Run(service.SignalContext())
	case "crawl":
		fs := flag.NewFlagSet("crawl", flag.ExitOnError)
		configPath := fs.String("config", "configs/example.yaml", "path to config file")
		rootID := fs.String("root", "", "root id to crawl; empty crawls all enabled roots")
		_ = fs.Parse(os.Args[2:])
		return service.App{ConfigPath: *configPath, Logger: log}.CrawlOnce(service.SignalContext(), *rootID)
	case "status":
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		configPath := fs.String("config", "configs/example.yaml", "path to config file")
		_ = fs.Parse(os.Args[2:])
		return service.App{ConfigPath: *configPath, Logger: log}.Status(service.SignalContext())
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
  qindexer version`)
}

//go:build !windows

package service

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"qindexer/internal/config"
)

type childPostgres struct {
	command *exec.Cmd
	done    <-chan error
	output  *os.File
}

func startPrivatePostgres(_ context.Context, cfg *config.Config, configPath, dataDir string, log *slog.Logger) (privatePostgresHandle, error) {
	private := cfg.Index.Database.PrivatePostgres
	if private == nil {
		return nil, nil
	}
	dataPath := private.DataDir
	if !filepath.IsAbs(dataPath) {
		dataPath = filepath.Join(filepath.Dir(configPath), dataPath)
	}
	if _, err := os.Stat(filepath.Join(dataPath, "PG_VERSION")); err != nil {
		return nil, fmt.Errorf("private PostgreSQL data directory is unavailable: %w", err)
	}
	address, err := postgresAddress(cfg.Index.Database.DSN)
	if err != nil {
		return nil, err
	}
	if postgresReady(address) {
		log.Debug("private PostgreSQL is already available", "address", address)
		return nil, nil
	}
	log.Info("starting private PostgreSQL", "data_dir", dataPath, "address", address)
	logPath := filepath.Join(dataDir, "logs", "private-postgres.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, fmt.Errorf("create private PostgreSQL log directory: %w", err)
	}
	output, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open private PostgreSQL log: %w", err)
	}
	var command *exec.Cmd
	if strings.TrimSpace(private.User) != "" {
		if os.Geteuid() != 0 {
			output.Close()
			return nil, fmt.Errorf("private PostgreSQL is configured to run as %q, but qindexer is not running as root", private.User)
		}
		command = exec.Command("runuser", "-u", private.User, "--", private.Command, "-D", dataPath)
	} else {
		command = exec.Command(private.Command, "-D", dataPath)
	}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		output.Close()
		return nil, fmt.Errorf("start private PostgreSQL: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	handle := &childPostgres{command: command, done: done, output: output}
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if postgresReady(address) {
			return handle, nil
		}
		select {
		case <-done:
			output.Close()
			return nil, fmt.Errorf("private PostgreSQL exited during startup; inspect %s", logPath)
		case <-time.After(250 * time.Millisecond):
		}
	}
	handle.stop(log)
	output.Close()
	return nil, fmt.Errorf("private PostgreSQL did not become ready within 30 seconds; inspect %s", logPath)
}

func postgresAddress(dsn string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse private PostgreSQL DSN: %w", err)
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if host != "127.0.0.1" && host != "::1" && host != "localhost" {
		return "", fmt.Errorf("private PostgreSQL requires a loopback DSN, got %q", parsed.Host)
	}
	if port == "" {
		port = "5432"
	}
	return net.JoinHostPort(host, port), nil
}

func postgresReady(address string) bool {
	connection, err := net.DialTimeout("tcp", address, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = connection.Close()
	return true
}

func (p *childPostgres) stop(log *slog.Logger) {
	if p == nil || p.command == nil || p.command.Process == nil {
		return
	}
	defer func() {
		if p.output != nil {
			_ = p.output.Close()
		}
	}()
	select {
	case <-p.done:
		return
	default:
	}
	if err := p.command.Process.Signal(os.Interrupt); err != nil {
		log.Warn("private PostgreSQL interrupt failed", "error", err)
	}
	select {
	case err := <-p.done:
		if err != nil && !strings.Contains(err.Error(), "signal: interrupt") {
			log.Warn("private PostgreSQL stopped with an error", "error", err)
		}
	case <-time.After(20 * time.Second):
		log.Warn("private PostgreSQL did not stop promptly; terminating it")
		_ = p.command.Process.Kill()
		<-p.done
	}
}

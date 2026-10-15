//go:build windows

package service

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"qindexer/internal/config"
)

func stopPrivatePostgres(configPath string, cfg *config.Config) error {
	if cfg.Index.Database.Driver != "postgres" || !strings.Contains(cfg.Index.Database.DSN, "@127.0.0.1:") {
		return nil
	}
	installDir := filepath.Dir(filepath.Dir(configPath))
	if _, err := exec.LookPath("sc.exe"); err != nil {
		return fmt.Errorf("sc.exe is unavailable: %w", err)
	}
	command := exec.Command("sc.exe", "stop", "qindexer-postgresql")
	command.Dir = installDir
	if output, err := command.CombinedOutput(); err != nil && !strings.Contains(string(output), "SERVICE_NOT_ACTIVE") {
		return fmt.Errorf("stop qindexer-postgresql: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

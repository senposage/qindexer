//go:build !windows

package api

import (
	"log/slog"

	"qindexer/internal/config"
)

func prepareSidecarEnvironment(_ *slog.Logger, _ config.OCRConfig) {}

package api

import (
	"log/slog"

	"qindexer/internal/config"
)

// PrepareSidecarEnvironment makes locally installed OCR commands available to
// this process before crawler workers begin using them.
func PrepareSidecarEnvironment(log *slog.Logger, settings config.OCRConfig) {
	prepareSidecarEnvironment(log, settings)
}

package extract

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type OCROptions struct {
	Engine           string
	TesseractCommand string
	OCRmyPDFCommand  string
	Languages        string
}

// OCREligible reports whether an empty extraction can be followed by local OCR.
func OCREligible(path string) bool {
	switch strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".") {
	case "pdf", "png", "jpg", "jpeg", "tif", "tiff", "bmp", "gif", "webp":
		return true
	default:
		return false
	}
}

// OCR invokes a locally installed engine. It never changes the indexed source.
func OCR(ctx context.Context, path string, options OCROptions) (string, string, error) {
	if !OCREligible(path) {
		return "unsupported", "", nil
	}
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	engine := strings.ToLower(strings.TrimSpace(options.Engine))
	if engine == "" || engine == "auto" {
		if ext == "pdf" {
			engine = "ocrmypdf"
		} else {
			engine = "tesseract"
		}
	}
	switch engine {
	case "tesseract":
		if ext == "pdf" {
			return "unsupported", "", errors.New("tesseract engine requires an image; use ocrmypdf or auto for PDFs")
		}
		return runTesseract(ctx, path, options)
	case "ocrmypdf":
		if ext != "pdf" {
			return "unsupported", "", errors.New("ocrmypdf engine only accepts PDFs")
		}
		return runOCRmyPDF(ctx, path, options)
	default:
		return "failed", "", fmt.Errorf("unknown OCR engine %q", options.Engine)
	}
}

func runTesseract(ctx context.Context, path string, options OCROptions) (string, string, error) {
	command := options.TesseractCommand
	if command == "" {
		command = "tesseract"
	}
	args := []string{path, "stdout"}
	if options.Languages != "" {
		args = append(args, "-l", options.Languages)
	}
	out, err := exec.CommandContext(ctx, command, args...).Output()
	if err != nil {
		return "failed", "", commandError(command, err)
	}
	return "extracted", strings.Join(strings.Fields(string(out)), " "), nil
}

func runOCRmyPDF(ctx context.Context, path string, options OCROptions) (string, string, error) {
	command := options.OCRmyPDFCommand
	if command == "" {
		command = "ocrmypdf"
	}
	dir, err := os.MkdirTemp("", "qindexer-ocr-*")
	if err != nil {
		return "failed", "", err
	}
	defer os.RemoveAll(dir)
	sidecar := filepath.Join(dir, "content.txt")
	args := []string{"--skip-text", "--output-type", "none", "--sidecar", sidecar}
	if options.Languages != "" {
		args = append(args, "-l", options.Languages)
	}
	// OCRmyPDF requires '-' as the output argument when output-type is none.
	args = append(args, path, "-")
	if out, err := exec.CommandContext(ctx, command, args...).CombinedOutput(); err != nil {
		return "failed", "", fmt.Errorf("%s failed: %w: %s", command, err, strings.TrimSpace(string(out)))
	}
	text, err := os.ReadFile(sidecar)
	if err != nil {
		return "failed", "", fmt.Errorf("%s did not produce a text sidecar: %w", command, err)
	}
	return "extracted", strings.Join(strings.Fields(string(text)), " "), nil
}

func commandError(command string, err error) error {
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("OCR command %q was not found", command)
	}
	return fmt.Errorf("%s failed: %w", command, err)
}

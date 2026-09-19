package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	var out cappedBuffer
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return "failed", "", commandError(command, err)
	}
	return "extracted", normalizeText(out.String()), nil
}

func runOCRmyPDF(ctx context.Context, path string, options OCROptions) (string, string, error) {
	command := options.OCRmyPDFCommand
	if command == "" {
		command = "ocrmypdf"
	}
	dir, err := os.MkdirTemp("", "qindexer-ocr-*")
	if err != nil {
		return "failed", "", errors.New("could not create OCR temporary workspace")
	}
	defer os.RemoveAll(dir)
	sidecar := filepath.Join(dir, "content.txt")
	args := []string{"--skip-text", "--output-type", "none", "--sidecar", sidecar}
	if options.Languages != "" {
		args = append(args, "-l", options.Languages)
	}
	// OCRmyPDF requires '-' as the output argument when output-type is none.
	args = append(args, path, "-")
	if err := exec.CommandContext(ctx, command, args...).Run(); err != nil {
		return "failed", "", commandError(command, err)
	}
	f, err := os.Open(sidecar)
	if err != nil {
		return "failed", "", fmt.Errorf("OCR command %q did not produce a text sidecar", commandName(command))
	}
	defer f.Close()
	text, err := io.ReadAll(io.LimitReader(f, maxTextBytes))
	if err != nil {
		return "failed", "", errors.New("could not read OCR text sidecar")
	}
	return "extracted", normalizeText(string(text)), nil
}

// cappedBuffer drains a command's output without allowing a malformed OCR
// sidecar or image to allocate an unbounded buffer in the service process.
type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	remaining := maxTextBytes - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return written, nil
}

func commandError(command string, err error) error {
	name := commandName(command)
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("OCR command %q was not found", name)
	}
	return fmt.Errorf("OCR command %q failed", name)
}

func commandName(command string) string {
	command = strings.TrimSpace(strings.ReplaceAll(command, "\\", "/"))
	if command == "" {
		return "OCR engine"
	}
	return filepath.Base(command)
}

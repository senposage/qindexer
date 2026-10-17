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

var ocrEligibleExtensions = []string{"pdf", "png", "jpg", "jpeg", "tif", "tiff", "bmp", "gif", "webp"}

// OCREligible reports whether an empty extraction can be followed by local OCR.
func OCREligible(path string) bool {
	switch strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".") {
	case "pdf", "png", "jpg", "jpeg", "tif", "tiff", "bmp", "gif", "webp":
		return true
	}
	return false
}

// OCREligibleExtensions reports file types that can be submitted to a local OCR engine.
func OCREligibleExtensions() []string {
	return append([]string(nil), ocrEligibleExtensions...)
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
	var stderr cappedBuffer
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "failed", "", commandError(command, err, stderr.String())
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
	if err := runOCRmyPDFCommand(ctx, command, path, sidecar, options.Languages); err != nil {
		if !shouldStageOCRInput(err) {
			return "failed", "", err
		}
		staged := filepath.Join(dir, "source.pdf")
		if stageErr := stageOCRInput(ctx, path, staged); stageErr != nil {
			return "failed", "", fmt.Errorf("%w; local staging retry could not read source: %v", err, stageErr)
		}
		if retryErr := runOCRmyPDFCommand(ctx, command, staged, sidecar, options.Languages); retryErr != nil {
			return "failed", "", fmt.Errorf("%w; local staging retry also failed: %v", err, retryErr)
		}
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

func runOCRmyPDFCommand(ctx context.Context, command, path, sidecar, languages string) error {
	args := []string{"--skip-text", "--output-type", "none", "--sidecar", sidecar}
	if languages != "" {
		args = append(args, "-l", languages)
	}
	// OCRmyPDF requires '-' as the output argument when output-type is none.
	args = append(args, path, "-")
	var stderr cappedBuffer
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return commandError(command, err, stderr.String())
	}
	return nil
}

// shouldStageOCRInput recognizes source-provider errors emitted before
// OCRmyPDF begins parsing the PDF. Staging only these cases avoids a second
// full read for ordinary local and network files.
func shouldStageOCRInput(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "invalid argument") ||
		strings.Contains(message, "cloud file") ||
		strings.Contains(message, "resource temporarily unavailable")
}

func stageOCRInput(ctx context.Context, source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer out.Close()
	buffer := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		read, readErr := in.Read(buffer)
		if read > 0 {
			if _, err := out.Write(buffer[:read]); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return out.Sync()
		}
		if readErr != nil {
			return readErr
		}
	}
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

func commandError(command string, err error, stderr string) error {
	name := commandName(command)
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("OCR command %q was not found", name)
	}
	if detail := commandDiagnostic(stderr); detail != "" {
		return fmt.Errorf("OCR command %q failed: %s", name, summarizeError(errors.New(detail)))
	}
	return fmt.Errorf("OCR command %q failed", name)
}

// commandDiagnostic selects the final non-empty stderr line. OCRmyPDF emits
// Python tracebacks where the final line contains the actionable exception;
// retaining the beginning would hide it behind stack frames.
func commandDiagnostic(stderr string) string {
	for lines := strings.Split(strings.ReplaceAll(stderr, "\r\n", "\n"), "\n"); len(lines) > 0; lines = lines[:len(lines)-1] {
		if detail := strings.TrimSpace(lines[len(lines)-1]); detail != "" {
			return detail
		}
	}
	return ""
}

func commandName(command string) string {
	command = strings.TrimSpace(strings.ReplaceAll(command, "\\", "/"))
	if command == "" {
		return "OCR engine"
	}
	return filepath.Base(command)
}

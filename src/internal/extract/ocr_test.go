package extract

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestOCREligible(t *testing.T) {
	for _, path := range []string{"scan.pdf", "photo.PNG", "receipt.tiff", "diagram.webp"} {
		if !OCREligible(path) {
			t.Fatalf("expected %q to be OCR eligible", path)
		}
	}
	for _, path := range []string{"report.docx", "notes.txt", "archive.zip"} {
		if OCREligible(path) {
			t.Fatalf("expected %q to be OCR ineligible", path)
		}
	}
}

func TestOCRRejectsUnsupportedEngineAndInputs(t *testing.T) {
	if status, _, err := OCR(t.Context(), "notes.txt", OCROptions{}); err != nil || status != "unsupported" {
		t.Fatalf("unsupported text result = %q, %v", status, err)
	}
	if status, _, err := OCR(t.Context(), "scan.pdf", OCROptions{Engine: "tesseract"}); err == nil || status != "unsupported" {
		t.Fatalf("PDF tesseract result = %q, %v", status, err)
	}
}

func TestCommandErrorDoesNotExposeConfiguredPath(t *testing.T) {
	configured := `Z:\private\tools\ocrmypdf.exe`
	err := commandError(configured, exec.ErrNotFound, "")
	if strings.Contains(err.Error(), configured) || strings.Contains(strings.ToLower(err.Error()), "private") {
		t.Fatalf("command error exposed configured path: %v", err)
	}
	if !strings.Contains(err.Error(), "ocrmypdf.exe") {
		t.Fatalf("command error = %v", err)
	}
}

func TestCommandErrorIncludesBoundedDiagnostic(t *testing.T) {
	err := commandError("ocrmypdf", errors.New("exit status 6"), "InputFileError: encrypted PDF")
	if !strings.Contains(err.Error(), "encrypted PDF") {
		t.Fatalf("command error discarded diagnostic: %v", err)
	}
	long := commandError("ocrmypdf", errors.New("exit status 1"), strings.Repeat("x", maxExtractionErrorLen+100))
	if !strings.HasSuffix(long.Error(), "[truncated]") {
		t.Fatalf("command error diagnostic was not bounded: %v", long)
	}
}

func TestCommandDiagnosticUsesFinalStderrLine(t *testing.T) {
	stderr := "Traceback (most recent call last):\n  File \"pipeline.py\", line 1\nOSError: [Errno 22] Invalid argument\n"
	if got := commandDiagnostic(stderr); got != "OSError: [Errno 22] Invalid argument" {
		t.Fatalf("diagnostic = %q", got)
	}
}

func TestShouldStageOCRInputOnlyForSourceProviderFailures(t *testing.T) {
	for _, value := range []string{
		"OCR command \"ocrmypdf\" failed: OSError: [Errno 22] Invalid argument",
		"Access to the cloud file is denied",
		"resource temporarily unavailable",
	} {
		if !shouldStageOCRInput(errors.New(value)) {
			t.Fatalf("expected staging fallback for %q", value)
		}
	}
	if shouldStageOCRInput(errors.New("invalid or incomplete PDF")) {
		t.Fatal("malformed PDFs must not receive a staging retry")
	}
}

package extract

import "testing"

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

package extract

import (
	"archive/zip"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/ledongthuc/pdf"
)

const maxTextBytes = 4 << 20

// Text returns best-effort text for common office, PDF, and plain-text files.
func Text(path string) (string, error) {
	ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
	switch ext {
	case "pdf":
		f, r, err := pdf.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		plain, err := r.GetPlainText()
		if err != nil {
			return "", err
		}
		return readText(plain)
	case "docx", "xlsx", "pptx":
		return officeText(path)
	case "png", "jpg", "jpeg", "tif", "tiff", "bmp", "gif", "webp":
		// Images have no embedded text. Returning an empty value hands them to OCR.
		return "", nil
	default:
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer f.Close()
		return readText(f)
	}
}

func officeText(path string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	var parts []string
	for _, f := range zr.File {
		name := strings.ToLower(f.Name)
		if !strings.HasSuffix(name, ".xml") || !(strings.HasPrefix(name, "word/") || strings.HasPrefix(name, "xl/") || strings.HasPrefix(name, "ppt/")) {
			continue
		}
		r, err := f.Open()
		if err != nil {
			continue
		}
		text, err := xmlText(r)
		r.Close()
		if err == nil {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " "), nil
}

func xmlText(r io.Reader) (string, error) {
	dec := xml.NewDecoder(io.LimitReader(r, maxTextBytes))
	var parts []string
	for {
		token, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		if chars, ok := token.(xml.CharData); ok {
			parts = append(parts, string(chars))
		}
	}
	return strings.Join(strings.Fields(strings.Join(parts, " ")), " "), nil
}

func readText(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxTextBytes))
	if err != nil {
		return "", err
	}
	return strings.Join(strings.Fields(string(b)), " "), nil
}

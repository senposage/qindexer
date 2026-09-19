package extract

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/ledongthuc/pdf"
)

const (
	// One MiB is enough for useful search and highlights while keeping one
	// unusually verbose document from dominating the on-disk index.
	maxTextBytes          = 1 << 20
	maxExtractionErrorLen = 320
)

var ErrUnsupported = errors.New("content extraction is unsupported for this file type")

var indexableExtensions = map[string]bool{
	"pdf": true, "docx": true, "xlsx": true, "pptx": true,
	"txt": true, "md": true, "csv": true, "tsv": true, "json": true,
	"yaml": true, "yml": true, "xml": true, "html": true, "htm": true,
	"log": true, "ini": true, "cfg": true, "conf": true, "sql": true,
	"ps1": true, "go": true, "js": true, "ts": true, "py": true,
	"rs": true, "java": true, "c": true, "h": true, "cpp": true, "cs": true,
	"png": true, "jpg": true, "jpeg": true, "tif": true, "tiff": true,
	"bmp": true, "gif": true, "webp": true,
}

func Indexable(path string) bool {
	return indexableExtensions[strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")]
}

func IndexableExtensions() []string {
	values := make([]string, 0, len(indexableExtensions))
	for ext := range indexableExtensions {
		values = append(values, ext)
	}
	return values
}

func withRecover(fn func() (string, error)) (text string, err error) {
	defer func() {
		if value := recover(); value != nil {
			text = ""
			err = fmt.Errorf("content extraction panic: %v", value)
		}
		if err != nil {
			err = summarizeError(err)
		}
	}()
	return fn()
}

// summarizeError keeps malformed input from injecting raw document data into
// service logs, API status, or the administration UI.
func summarizeError(err error) error {
	message := err.Error()
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "malformed hex string"), strings.Contains(lower, "unexpected delimiter"), strings.Contains(lower, "malformed name"):
		return errors.New("malformed PDF content")
	case strings.Contains(lower, "missing %%eof"), strings.Contains(lower, "not a pdf file"):
		return errors.New("invalid or incomplete PDF")
	case strings.Contains(lower, "encrypted pdf"):
		return errors.New("encrypted PDF cannot be read")
	}

	var builder strings.Builder
	for _, r := range message {
		if unicode.IsPrint(r) {
			builder.WriteRune(r)
		} else if unicode.IsSpace(r) {
			builder.WriteByte(' ')
		} else {
			builder.WriteByte('?')
		}
		if builder.Len() >= maxExtractionErrorLen {
			return errors.New(strings.TrimSpace(builder.String()) + " [truncated]")
		}
	}
	return errors.New(strings.TrimSpace(builder.String()))
}

// Text returns best-effort text for common office, PDF, and plain-text files.
func Text(path string) (string, error) {
	return withRecover(func() (string, error) {
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
		if !Indexable(path) {
			return "", ErrUnsupported
		}
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
	})
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
	return normalizeText(strings.Join(parts, " ")), nil
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
	return normalizeText(strings.Join(parts, " ")), nil
}

func readText(r io.Reader) (string, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxTextBytes))
	if err != nil {
		return "", err
	}
	return normalizeText(string(b)), nil
}

// normalizeText keeps stored search text bounded after whitespace cleanup. The
// byte slice boundary is adjusted so valid UTF-8 is never split in half.
func normalizeText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= maxTextBytes {
		return value
	}
	limit := maxTextBytes
	for limit > 0 && (value[limit]&0xc0) == 0x80 {
		limit--
	}
	return value[:limit]
}

// TruncateText applies a caller-selected storage cap after normalization. A
// non-positive cap uses the extractor's conservative default.
func TruncateText(value string, maxBytes int64) string {
	value = normalizeText(value)
	if maxBytes <= 0 || maxBytes >= int64(len(value)) {
		return value
	}
	limit := int(maxBytes)
	for limit > 0 && (value[limit]&0xc0) == 0x80 {
		limit--
	}
	return value[:limit]
}

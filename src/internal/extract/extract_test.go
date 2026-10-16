package extract

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWithRecoverReturnsPanicAsError(t *testing.T) {
	text, err := withRecover(func() (string, error) {
		panic("malformed pdf")
	})
	if text != "" {
		t.Fatalf("text = %q, want empty", text)
	}
	if err == nil || !strings.Contains(err.Error(), "malformed pdf") {
		t.Fatalf("error = %v, want panic details", err)
	}
}

func TestSummarizeErrorRedactsParserPayload(t *testing.T) {
	for _, message := range []string{
		"malformed hex string " + strings.Repeat("raw-pdf-bytes", 100) + "\x00",
		"unexpected delimiter ')'",
	} {
		err := summarizeError(errors.New(message))
		if got := err.Error(); got != "malformed PDF content" {
			t.Fatalf("error = %q, want a concise malformed PDF summary", got)
		}
	}
}

func TestSummarizeErrorBoundsUnknownErrors(t *testing.T) {
	err := summarizeError(errors.New(strings.Repeat("x", maxExtractionErrorLen+100) + "\x00"))
	if len(err.Error()) > maxExtractionErrorLen+len(" [truncated]") || !strings.HasSuffix(err.Error(), "[truncated]") {
		t.Fatalf("unbounded error summary: %q", err)
	}
}

func TestNormalizeTextRepairsInvalidUTF8(t *testing.T) {
	got := normalizeText("alpha\x80 beta")
	if !utf8.ValidString(got) {
		t.Fatalf("normalized text is not valid UTF-8: %q", got)
	}
	if got != "alpha\uFFFD beta" {
		t.Fatalf("normalized text = %q, want replacement character", got)
	}
}

func TestNormalizeTextReplacesNULBytes(t *testing.T) {
	got := normalizeText("alpha\x00 beta")
	if strings.ContainsRune(got, '\x00') {
		t.Fatalf("normalized text retained NUL byte: %q", got)
	}
	if got != "alpha\uFFFD beta" {
		t.Fatalf("normalized text = %q, want replacement character", got)
	}
}

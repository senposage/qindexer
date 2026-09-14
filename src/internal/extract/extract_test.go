package extract

import (
	"errors"
	"strings"
	"testing"
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

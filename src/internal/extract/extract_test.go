package extract

import (
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

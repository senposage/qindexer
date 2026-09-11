package config

import "testing"

func TestRootEnrichmentOverridesGlobalDefaults(t *testing.T) {
	trueValue, falseValue := true, false
	root := RootConfig{ContentExtraction: &trueValue, OCR: &falseValue, Hashing: &falseValue, CollectOwnership: &trueValue}
	if !root.ContentExtractionEnabled(false) || root.OCREnabled(true) || root.HashingEnabled(true) || !root.OwnershipEnabled(false) {
		t.Fatalf("unexpected root overrides: %#v", root)
	}
	if !(RootConfig{}).ContentExtractionEnabled(true) || (RootConfig{}).HashingEnabled(false) {
		t.Fatal("unset root settings should inherit global defaults")
	}
}

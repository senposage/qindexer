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

func TestBindAddressesDefaultFromLegacyBind(t *testing.T) {
	cfg := Config{Server: ServerConfig{Bind: "127.0.0.1:41973"}, Management: ManagementConfig{Bind: "127.0.0.1:41974"}}
	cfg.ApplyDefaults()
	if len(cfg.Server.BindAddresses) != 1 || cfg.Server.BindAddresses[0] != "127.0.0.1:41973" {
		t.Fatalf("server bind addresses did not inherit legacy bind: %#v", cfg.Server.BindAddresses)
	}
	if len(cfg.Management.BindAddresses) != 1 || cfg.Management.BindAddresses[0] != "127.0.0.1:41974" {
		t.Fatalf("management bind addresses did not inherit legacy bind: %#v", cfg.Management.BindAddresses)
	}
}

func TestBindAddressesValidateHostPort(t *testing.T) {
	cfg := Config{Server: ServerConfig{BindAddresses: []string{"127.0.0.1"}}, Management: ManagementConfig{BindAddresses: []string{"127.0.0.1:41974"}}}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected bind address without port to fail")
	}
}

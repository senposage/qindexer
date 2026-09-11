//go:build !windows

package filemeta

// Owner is deliberately best-effort. Platform-specific ownership collection can
// be added without changing the index contract.
func Owner(path string) string { return "" }

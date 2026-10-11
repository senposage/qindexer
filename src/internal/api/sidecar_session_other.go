//go:build !windows

package api

func windowsCanShowElevationPrompt() bool {
	return true
}

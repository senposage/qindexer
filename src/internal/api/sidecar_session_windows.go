//go:build windows

package api

import (
	"os"

	"golang.org/x/sys/windows"
)

func windowsCanShowElevationPrompt() bool {
	var sessionID uint32
	if err := windows.ProcessIdToSessionId(uint32(os.Getpid()), &sessionID); err != nil {
		// Preserve the prior behavior when Windows cannot report a session.
		return true
	}
	return sessionID != 0
}

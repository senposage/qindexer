package filemeta

import "golang.org/x/sys/windows"

// Owner returns the Windows SID without attempting account-name resolution.
// SID lookup is stable and avoids a potentially slow domain-controller call.
func Owner(path string) string {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return ""
	}
	sid, _, err := sd.Owner()
	if err != nil || sid == nil {
		return ""
	}
	return sid.String()
}

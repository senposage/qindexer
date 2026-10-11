//go:build windows

package crawler

import (
	"io/fs"

	"golang.org/x/sys/windows"
)

// Windows directory junctions do not consistently present as ModeSymlink in
// DirEntry. Check the native reparse attribute before queueing them.
func isReparsePoint(path string, entry fs.DirEntry) bool {
	if entry.Type()&fs.ModeSymlink != 0 {
		return true
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	attributes, err := windows.GetFileAttributes(pointer)
	return err == nil && attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

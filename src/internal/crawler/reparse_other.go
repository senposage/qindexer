//go:build !windows

package crawler

import "io/fs"

func isReparsePoint(_ string, entry fs.DirEntry) bool {
	return entry.Type()&fs.ModeSymlink != 0
}

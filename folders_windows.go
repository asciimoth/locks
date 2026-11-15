//go:build windows

package locks

import "golang.org/x/sys/windows"

func knownFolders() (string, string) {
	a, _ := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	b, _ := windows.KnownFolderPath(windows.FOLDERID_ProgramData, 0)
	return a, b
}

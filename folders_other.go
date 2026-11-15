//go:build !windows

package locks

func knownFolders() (string, string) { return "", "" }

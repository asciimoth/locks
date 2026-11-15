//go:build linux || openbsd

package locks

import "os"

func clearInheritedACL(*os.File) error { return nil }

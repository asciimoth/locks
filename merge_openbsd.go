package locks

import "os"

func mergeExtraMetadata(_ *os.File, _ bool, out mergeAccess) (mergeAccess, error) {
	return out, nil
}

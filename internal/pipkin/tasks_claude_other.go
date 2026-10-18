//go:build !darwin && !windows

package pipkin

import "errors"

func ownedProcessInfo(int) (processIdentity, error) {
	return processIdentity{}, errors.New("Claude task collection is unsupported")
}

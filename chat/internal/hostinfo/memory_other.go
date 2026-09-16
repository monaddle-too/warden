//go:build !linux && !darwin

package hostinfo

import "errors"

func totalMemory() (uint64, error) {
	return 0, errors.New("host memory detection supports Linux and macOS only")
}

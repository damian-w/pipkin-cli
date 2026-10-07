package pipkin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type startupFile struct {
	path    string
	data    []byte
	mode    os.FileMode
	link    string
	present bool
}

func captureStartupFile(path string) (startupFile, error) {
	state := startupFile{path: path}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.present, state.mode = true, info.Mode()
	if info.Mode()&os.ModeSymlink != 0 {
		state.link, err = os.Readlink(path)
	} else if info.Mode().IsRegular() {
		state.data, err = os.ReadFile(path)
	} else {
		err = fmt.Errorf("startup definition %s is not a regular file or symlink", path)
	}
	return state, err
}

func (state startupFile) restore() error {
	if !state.present {
		return removeIfExists(state.path)
	}
	if state.mode&os.ModeSymlink != 0 {
		if err := removeIfExists(state.path); err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(state.path), 0755); err != nil {
			return err
		}
		return os.Symlink(state.link, state.path)
	}
	return writeFileAtomic(state.path, state.data, state.mode.Perm())
}

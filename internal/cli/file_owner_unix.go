//go:build unix

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// fileOwner reports the uid and gid of the file path names. present is false
// when there is no file yet, so a first write has no owner to keep.
func fileOwner(path string) (int, int, bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, 0, false, nil
	}

	if err != nil {
		return 0, 0, false, fmt.Errorf("stat %s for its owner: %w", path, err)
	}

	stat, isUnix := info.Sys().(*syscall.Stat_t)
	if !isUnix {
		return 0, 0, false, nil
	}

	return int(stat.Uid), int(stat.Gid), true, nil
}

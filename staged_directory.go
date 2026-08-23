package ethertest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type emptyDirectoryTarget struct {
	existed bool
	mode    os.FileMode
}

func inspectEmptyDirectoryTarget(path string) (emptyDirectoryTarget, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return emptyDirectoryTarget{mode: 0o755}, nil
	}
	if err != nil {
		return emptyDirectoryTarget{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return emptyDirectoryTarget{}, fmt.Errorf("destination must be an empty directory, not %s", info.Mode().Type())
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return emptyDirectoryTarget{}, err
	}
	if len(entries) != 0 {
		return emptyDirectoryTarget{}, errors.New("destination must be empty")
	}
	return emptyDirectoryTarget{existed: true, mode: info.Mode().Perm()}, nil
}

// installStagedDirectory installs a fully closed sibling directory. POSIX can
// atomically replace an empty directory. Windows requires removing the empty
// placeholder first, so failures explicitly restore it. The returned rollback
// remains available until the caller has completed any reopen verification.
func installStagedDirectory(staging, destination string, target emptyDirectoryTarget) (func() error, error) {
	parent := filepath.Dir(destination)
	restoreEmpty := func() error {
		if !target.existed {
			return nil
		}
		return os.Mkdir(destination, target.mode)
	}
	removedPlaceholder := false
	renameErr := os.Rename(staging, destination)
	if renameErr != nil && target.existed {
		if err := os.Remove(destination); err != nil {
			return nil, errors.Join(renameErr, err)
		}
		removedPlaceholder = true
		renameErr = os.Rename(staging, destination)
	}
	if renameErr != nil {
		if removedPlaceholder {
			renameErr = errors.Join(renameErr, restoreEmpty())
		}
		return nil, renameErr
	}
	rollback := func() error {
		removeErr := os.RemoveAll(destination)
		restoreErr := restoreEmpty()
		syncErr := syncDirectory(parent)
		return errors.Join(removeErr, restoreErr, syncErr)
	}
	if err := syncDirectory(parent); err != nil {
		return nil, errors.Join(err, rollback())
	}
	return rollback, nil
}

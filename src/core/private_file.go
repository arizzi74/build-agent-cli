package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// writePrivateFile atomically replaces a regular private file without following a
// final-path symlink. Profile state is deliberately private even when an older
// CLI version created it with a broader mode.
func writePrivateFile(path string, data []byte) error {
	if err := rejectSymlinkPath(path); err != nil {
		return errors.New("refusing unsafe credential storage path")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("refusing unsafe credential storage path")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(dir, ".private-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return nil
}

func privateFileError(path string) error {
	return fmt.Errorf("could not securely persist %s", filepath.Base(path))
}

//go:build !windows

package graders

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// programContextFile is a grading context file in a private (0700) directory.
// The creating descriptor stays open until cleanup so the file's identity
// cannot be reused, and removal happens relative to the retained directory
// handle only after verifying the entry is still that file. Unix has no
// unlink-by-descriptor, so a replacement made by a still-running process
// between verification and unlink is not prevented.
type programContextFile struct {
	dir   string
	path  string
	root  *os.Root
	guard *os.File
	info  os.FileInfo
	hooks programContextHooks
}

func createProgramContextFile(data []byte, hooks programContextHooks) (_ *programContextFile, err error) {
	dir, err := os.MkdirTemp("", "waza-grading-context-")
	if err != nil {
		return nil, err
	}
	f := &programContextFile{dir: dir, path: filepath.Join(dir, programContextFileName), hooks: hooks}
	defer func() {
		if err != nil {
			err = errors.Join(err, f.remove())
		}
	}()
	if hooks.created != nil {
		hooks.created(f.path)
	}
	if f.root, err = os.OpenRoot(dir); err != nil {
		return nil, err
	}
	dirInfo, err := f.root.Stat(".")
	if err != nil {
		return nil, err
	}
	if !dirInfo.IsDir() || dirInfo.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("grading context directory %s is not private (mode %v)", dir, dirInfo.Mode().Perm())
	}
	if f.guard, err = f.root.OpenFile(programContextFileName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
		return nil, err
	}
	if f.info, err = f.guard.Stat(); err != nil {
		return nil, err
	}
	if err = hooks.doWrite(f.guard.Write, data); err != nil {
		return nil, err
	}
	return f, nil
}

// remove deletes the verified context file and its directory. Anything it
// cannot verify as its own is left in place and reported with its path.
func (f *programContextFile) remove() error {
	var errs []error
	removed := f.guard == nil // nothing was created inside the directory
	if f.guard != nil {
		if f.hooks.beforeRemove != nil {
			f.hooks.beforeRemove(f.path)
		}
		removed = f.removeVerified(&errs)
		if err := f.hooks.doCloseGuard(f.guard.Close); err != nil {
			errs = append(errs, fmt.Errorf("closing grading context file %s: %w", f.path, err))
		}
		f.guard = nil
	}
	if f.root != nil {
		if err := f.root.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing grading context directory %s: %w", f.dir, err))
		}
		f.root = nil
	}
	if removed {
		if err := os.Remove(f.dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("removing grading context directory %s (left in place): %w", f.dir, err))
		}
	} else {
		errs = append(errs, fmt.Errorf("grading context directory %s was left in place", f.dir))
	}
	return errors.Join(errs...)
}

func (f *programContextFile) removeVerified(errs *[]error) bool {
	if f.info == nil {
		*errs = append(*errs, fmt.Errorf("grading context file %s identity is unknown; it was left in place", f.path))
		return false
	}
	current, err := f.root.Lstat(programContextFileName)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true
	case err != nil:
		*errs = append(*errs, fmt.Errorf("inspecting grading context file %s: %w", f.path, err))
		return false
	case !os.SameFile(f.info, current):
		*errs = append(*errs, fmt.Errorf("grading context file %s was replaced; the replacement was left in place", f.path))
		return false
	}
	if err := f.hooks.doRemove(func() error { return f.root.Remove(programContextFileName) }); err != nil {
		*errs = append(*errs, fmt.Errorf("removing grading context file %s (left in place): %w", f.path, err))
		return false
	}
	return true
}

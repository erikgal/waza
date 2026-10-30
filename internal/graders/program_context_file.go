package graders

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// programContextFileName is the context file's name inside its private directory.
const programContextFileName = "context.json"

// programContextFile is a grading context file in a new private directory.
//
// Trust model: the grader program and other processes running as the same
// user are trusted; the program can read and copy the context anyway. The
// private directory and file protect against other local users, and cleanup
// errors fail grading so residue is never left silently. Races by same-user
// processes are out of scope.
type programContextFile struct {
	dir   string
	path  string
	hooks programContextHooks
}

// programContextHooks wrap individual context-file steps so tests can inject
// deterministic failures per grader, without package globals.
type programContextHooks struct {
	created func(path string)
	write   func(write func([]byte) (int, error), data []byte) (int, error)
	close   func(close func() error) error
	remove  func(remove func() error) error
}

func createProgramContextFile(data []byte, hooks programContextHooks) (_ *programContextFile, err error) {
	dir, err := mkdirPrivateTemp("waza-grading-context-")
	if err != nil {
		return nil, fmt.Errorf("creating private grading context directory: %w", err)
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
	file, err := createPrivateFile(f.path)
	if err != nil {
		return nil, fmt.Errorf("creating grading context file %s: %w", f.path, err)
	}
	writeErr := hooks.doWrite(file.Write, data)
	closeErr := hooks.doClose(file.Close)
	if writeErr != nil {
		writeErr = fmt.Errorf("writing grading context file %s: %w", f.path, writeErr)
	}
	if closeErr != nil {
		closeErr = fmt.Errorf("closing grading context file %s: %w", f.path, closeErr)
	}
	if err := errors.Join(writeErr, closeErr); err != nil {
		return nil, err
	}
	return f, nil
}

// remove deletes the context directory and everything in it. A failure
// reports the directory so remaining evidence can be located.
func (f *programContextFile) remove() error {
	err := f.hooks.doRemove(func() error { return os.RemoveAll(f.dir) })
	if err != nil {
		return fmt.Errorf("removing grading context directory %s (transcript data may remain): %w", f.dir, err)
	}
	return nil
}

func (h programContextHooks) doWrite(write func([]byte) (int, error), data []byte) error {
	var n int
	var err error
	if h.write != nil {
		n, err = h.write(write, data)
	} else {
		n, err = write(data)
	}
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func (h programContextHooks) doClose(close func() error) error {
	if h.close != nil {
		return h.close(close)
	}
	return close()
}

func (h programContextHooks) doRemove(remove func() error) error {
	if h.remove != nil {
		return h.remove(remove)
	}
	return remove()
}

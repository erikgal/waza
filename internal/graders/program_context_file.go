package graders

import "io"

// programContextFileName is the context file's name inside its private directory.
const programContextFileName = "context.json"

// programContextHooks wrap individual context-file steps so tests can inject
// deterministic failures and replacements per grader, without package globals.
type programContextHooks struct {
	beforeCreateDir func(dir string)
	created         func(path string)
	write           func(write func([]byte) (int, error), data []byte) (int, error)
	closeGuard      func(close func() error) error
	beforeRemove    func(path string)
	remove          func(remove func() error) error
}

func (h programContextHooks) doWrite(write func([]byte) (int, error), data []byte) error {
	n, err := 0, error(nil)
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

func (h programContextHooks) doCloseGuard(close func() error) error {
	if h.closeGuard != nil {
		return h.closeGuard(close)
	}
	return close()
}

func (h programContextHooks) doRemove(remove func() error) error {
	if h.remove != nil {
		return h.remove(remove)
	}
	return remove()
}

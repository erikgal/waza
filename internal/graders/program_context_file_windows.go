//go:build windows

package graders

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// programContextFile is a grading context file in a directory whose protected
// DACL grants access only to the current user. While the grader program runs,
// Waza holds a read handle without FILE_SHARE_DELETE, so the file cannot be
// deleted or renamed. Cleanup reopens the path, verifies the file identity,
// and deletes through that verified handle.
type programContextFile struct {
	dir   string
	path  string
	guard windows.Handle
	id    programContextFileID
	hooks programContextHooks
}

type programContextFileID struct {
	volume, high, low uint32
}

func createProgramContextFile(data []byte, hooks programContextHooks) (_ *programContextFile, err error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, fmt.Errorf("resolving current user for private grading context: %w", err)
	}
	dirSA, err := ownerOnlySecurityAttributes("D:P(A;OICI;FA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}
	fileSA, err := ownerOnlySecurityAttributes("D:P(A;;FA;;;" + sid + ")")
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(os.TempDir(), "waza-grading-context-"+rand.Text())
	dirPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return nil, err
	}
	if err := windows.CreateDirectory(dirPtr, dirSA); err != nil {
		return nil, fmt.Errorf("creating private grading context directory %s: %w", dir, err)
	}
	f := &programContextFile{dir: dir, path: filepath.Join(dir, programContextFileName), guard: windows.InvalidHandle, hooks: hooks}
	created := false
	defer func() {
		if err != nil {
			err = errors.Join(err, f.cleanup(created))
		}
	}()
	if hooks.created != nil {
		hooks.created(f.path)
	}

	pathPtr, err := windows.UTF16PtrFromString(f.path)
	if err != nil {
		return nil, err
	}
	writer, err := windows.CreateFile(pathPtr, windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ,
		fileSA, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, fmt.Errorf("creating grading context file %s: %w", f.path, err)
	}
	created = true
	writeErr := f.recordIdentity(writer)
	if writeErr == nil {
		writeErr = hooks.doWrite(func(b []byte) (int, error) {
			var n uint32
			err := windows.WriteFile(writer, b, &n, nil)
			return int(n), err
		}, data)
	}
	if writeErr == nil {
		// Read-only guard: readers may use FILE_SHARE_READ only (.NET defaults),
		// while omitting FILE_SHARE_DELETE blocks deletion and renaming.
		f.guard, writeErr = windows.CreateFile(pathPtr, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if writeErr != nil {
			f.guard = windows.InvalidHandle
		}
	}
	if closeErr := windows.CloseHandle(writer); closeErr != nil {
		writeErr = errors.Join(writeErr, fmt.Errorf("closing grading context writer: %w", closeErr))
	}
	if writeErr != nil {
		return nil, writeErr
	}
	return f, nil
}

func (f *programContextFile) recordIdentity(h windows.Handle) error {
	id, err := handleFileID(h)
	if err != nil {
		return fmt.Errorf("inspecting grading context file %s: %w", f.path, err)
	}
	f.id = id
	return nil
}

// remove deletes the verified context file and its directory. Anything it
// cannot verify as its own is left in place and reported with its path.
func (f *programContextFile) remove() error {
	return f.cleanup(true)
}

func (f *programContextFile) cleanup(created bool) error {
	var errs []error
	if f.guard != windows.InvalidHandle {
		guard := f.guard
		f.guard = windows.InvalidHandle
		if err := f.hooks.doCloseGuard(func() error { return windows.CloseHandle(guard) }); err != nil {
			errs = append(errs, fmt.Errorf("closing grading context file %s: %w", f.path, err))
		}
	}
	removed := !created
	if created {
		if f.hooks.beforeRemove != nil {
			f.hooks.beforeRemove(f.path)
		}
		removed = f.removeVerified(&errs)
	}
	if removed {
		dirPtr, err := windows.UTF16PtrFromString(f.dir)
		if err == nil {
			err = windows.RemoveDirectory(dirPtr)
		}
		if err != nil && !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			errs = append(errs, fmt.Errorf("removing grading context directory %s (left in place): %w", f.dir, err))
		}
	} else {
		errs = append(errs, fmt.Errorf("grading context directory %s was left in place", f.dir))
	}
	return errors.Join(errs...)
}

func (f *programContextFile) removeVerified(errs *[]error) bool {
	if f.id == (programContextFileID{}) {
		*errs = append(*errs, fmt.Errorf("grading context file %s identity is unknown; it was left in place", f.path))
		return false
	}
	pathPtr, err := windows.UTF16PtrFromString(f.path)
	if err != nil {
		*errs = append(*errs, err)
		return false
	}
	h, err := windows.CreateFile(pathPtr, windows.DELETE|windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return true
	}
	if err != nil {
		*errs = append(*errs, fmt.Errorf("opening grading context file %s for removal (left in place): %w", f.path, err))
		return false
	}
	defer func() {
		if err := windows.CloseHandle(h); err != nil {
			*errs = append(*errs, fmt.Errorf("closing grading context file %s after removal: %w", f.path, err))
		}
	}()
	id, err := handleFileID(h)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("inspecting grading context file %s (left in place): %w", f.path, err))
		return false
	}
	if id != f.id {
		*errs = append(*errs, fmt.Errorf("grading context file %s was replaced; the replacement was left in place", f.path))
		return false
	}
	if err := f.hooks.doRemove(func() error {
		deleteFile := byte(1) // FILE_DISPOSITION_INFO.DeleteFile
		return windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, &deleteFile, uint32(unsafe.Sizeof(deleteFile)))
	}); err != nil {
		*errs = append(*errs, fmt.Errorf("removing grading context file %s (left in place): %w", f.path, err))
		return false
	}
	return true
}

func handleFileID(h windows.Handle) (programContextFileID, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return programContextFileID{}, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return programContextFileID{}, errors.New("grading context entry is a reparse point")
	}
	return programContextFileID{info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow}, nil
}

func currentUserSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func ownerOnlySecurityAttributes(sddl string) (*windows.SecurityAttributes, error) {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, fmt.Errorf("building owner-only security descriptor: %w", err)
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, nil
}

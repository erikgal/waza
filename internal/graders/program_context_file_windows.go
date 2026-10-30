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
// DACL grants access only to the current user. Waza holds a directory handle
// without FILE_SHARE_DELETE from creation through cleanup, so the directory
// cannot be renamed or replaced, and finally deletes it through that handle.
// While the grader program runs, a file read handle without FILE_SHARE_DELETE
// prevents deleting or renaming the file. Cleanup reopens the file inside the
// retained directory, verifies its identity, and deletes through that handle.
type programContextFile struct {
	dir      string
	path     string
	dirGuard windows.Handle
	guard    windows.Handle
	id       programContextFileID
	hooks    programContextHooks
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

	parent := os.TempDir()
	name := "waza-grading-context-" + rand.Text()
	if hooks.beforeCreateDir != nil {
		hooks.beforeCreateDir(filepath.Join(parent, name))
	}
	dirGuard, err := createOwnedDirectory(parent, name, dirSA.SecurityDescriptor)
	if err != nil {
		return nil, fmt.Errorf("creating private grading context directory %s: %w", filepath.Join(parent, name), err)
	}
	dir := filepath.Join(parent, name)
	f := &programContextFile{dir: dir, path: filepath.Join(dir, programContextFileName), dirGuard: dirGuard, guard: windows.InvalidHandle, hooks: hooks}
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
	id, _, err := handleFileInfo(h)
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
		// The guard blocks renaming but not hard links; count names while held.
		if _, links, err := handleFileInfo(guard); err != nil {
			errs = append(errs, fmt.Errorf("inspecting grading context file %s: %w", f.path, err))
		} else if links > 1 {
			errs = append(errs, fmt.Errorf("grading context file %s was linked elsewhere; %d other name(s) with transcript data remain at unknown paths", f.path, links-1))
		}
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
	if f.dirGuard == windows.InvalidHandle {
		// Without a verified handle the directory cannot be removed safely.
		return errors.Join(append(errs, fmt.Errorf("grading context directory %s was left in place", f.dir))...)
	}
	dirGuard := f.dirGuard
	f.dirGuard = windows.InvalidHandle
	if removed {
		if f.hooks.beforeRemoveDir != nil {
			f.hooks.beforeRemoveDir(f.dir)
		}
		deleteDir := byte(1) // FILE_DISPOSITION_INFO.DeleteFile
		if err := windows.SetFileInformationByHandle(dirGuard, windows.FileDispositionInfo, &deleteDir, uint32(unsafe.Sizeof(deleteDir))); err != nil {
			errs = append(errs, fmt.Errorf("removing grading context directory %s (left in place): %w", f.dir, err))
		}
	} else {
		errs = append(errs, fmt.Errorf("grading context directory %s was left in place", f.dir))
	}
	if err := windows.CloseHandle(dirGuard); err != nil {
		errs = append(errs, fmt.Errorf("closing grading context directory %s: %w", f.dir, err))
	}
	return errors.Join(errs...)
}

// createOwnedDirectory creates name inside parent and returns a handle to the
// new directory in one NtCreateFile call (FILE_CREATE), so the handle can only
// refer to the directory this call created; an existing entry of any kind is
// rejected and never opened. The handle omits FILE_SHARE_DELETE, so the
// directory cannot be renamed or deleted by others while it is held.
func createOwnedDirectory(parent, name string, sd *windows.SECURITY_DESCRIPTOR) (windows.Handle, error) {
	parentPtr, err := windows.UTF16PtrFromString(parent)
	if err != nil {
		return windows.InvalidHandle, err
	}
	const fileAddSubdirectory = 0x0004 // FILE_ADD_SUBDIRECTORY; not exported by x/sys/windows
	parentHandle, err := windows.CreateFile(parentPtr, windows.FILE_TRAVERSE|fileAddSubdirectory,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return windows.InvalidHandle, fmt.Errorf("opening temporary directory: %w", err)
	}
	defer windows.CloseHandle(parentHandle) //nolint:errcheck // read-only parent handle

	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attrs := windows.OBJECT_ATTRIBUTES{RootDirectory: parentHandle, ObjectName: objectName, Attributes: windows.OBJ_CASE_INSENSITIVE, SecurityDescriptor: sd}
	attrs.Length = uint32(unsafe.Sizeof(attrs))
	var handle windows.Handle
	var iosb windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(&handle,
		windows.DELETE|windows.FILE_LIST_DIRECTORY|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		&attrs, &iosb, nil, windows.FILE_ATTRIBUTE_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, windows.FILE_CREATE,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0); err != nil {
		return windows.InvalidHandle, err
	}
	return handle, nil
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
		// Deletion and renaming were blocked while the program ran, so a missing
		// file was moved or deleted by a process that outlived it. Without a
		// handle Waza cannot tell which, so removal is reported as unverified.
		*errs = append(*errs, fmt.Errorf("grading context file %s disappeared after the grader exited; it may remain elsewhere with transcript data", f.path))
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
	id, _, err := handleFileInfo(h)
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
	// Count names only after the delete is pending: new hard links to a
	// delete-pending file are refused, so no name can appear after this check.
	// The count already excludes this delete-pending name, so any link left is
	// another name.
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		*errs = append(*errs, fmt.Errorf("inspecting removed grading context file %s; its removal cannot be verified: %w", f.path, err))
	} else if info.NumberOfLinks > 0 {
		*errs = append(*errs, fmt.Errorf("grading context file %s has %d other name(s) with transcript data at unknown paths", f.path, info.NumberOfLinks))
	}
	return true
}

func handleFileInfo(h windows.Handle) (programContextFileID, uint32, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return programContextFileID{}, 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return programContextFileID{}, 0, errors.New("grading context entry is a reparse point")
	}
	return programContextFileID{info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow}, info.NumberOfLinks, nil
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

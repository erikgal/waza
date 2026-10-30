//go:build !windows

package graders

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func assertPrivateContextFile(t *testing.T, path string) {
	t.Helper()
	dir, err := os.Lstat(filepath.Dir(path))
	require.NoError(t, err)
	require.True(t, dir.IsDir())
	require.Equal(t, os.FileMode(0o700), dir.Mode().Perm())
	file, err := os.Lstat(path)
	require.NoError(t, err)
	require.True(t, file.Mode().IsRegular())
	require.Equal(t, os.FileMode(0o600), file.Mode().Perm())
}

func openContextShareReadOnly(string) error { return nil }

func TestProgramContextFile_DirectoryDescriptorHeldThroughDirectoryRemoval(t *testing.T) {
	var f *programContextFile
	checked := false
	hooks := programContextHooks{
		beforeRemoveDir: func(dir string) {
			require.NotNil(t, f.root, "the directory descriptor must stay open so its inode cannot be reused")
			info, err := f.root.Stat(".")
			require.NoError(t, err)
			require.True(t, os.SameFile(f.dirInfo, info))
			checked = true
		},
	}
	var err error
	f, err = createProgramContextFile([]byte("{}"), hooks)
	require.NoError(t, err)
	require.NoError(t, f.remove())
	require.True(t, checked)
	require.NoDirExists(t, f.dir)
	require.Nil(t, f.root, "the descriptor is released after removal")
}

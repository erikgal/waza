package graders

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

// gradeWithContextHooks runs the helper program with per-grader hooks and
// returns the grade result, error and the context file path Waza created.
func gradeWithContextHooks(t *testing.T, mode string, hooks programContextHooks) (*Context, string, error) {
	t.Helper()
	g := newProgramContextHelperGrader(t, mode)
	var path string
	created := hooks.created
	hooks.created = func(p string) {
		path = p
		t.Cleanup(func() { require.NoError(t, os.RemoveAll(filepath.Dir(p))) })
		if created != nil {
			created(p)
		}
	}
	g.contextHooks = hooks
	gradingContext := &Context{Output: "out", WorkspaceDir: t.TempDir()}
	_, err := g.Grade(t.Context(), gradingContext)
	require.NotEmpty(t, path, "context file must have been created")
	return gradingContext, path, err
}

func TestProgramContextFile_IsPrivateWhileProgramRuns(t *testing.T) {
	checked := false
	_, path, err := gradeWithContextHooks(t, "pass", programContextHooks{
		beforeRemove: func(p string) {
			assertPrivateContextFile(t, p)
			checked = true
		},
	})
	require.NoError(t, err)
	require.True(t, checked)
	require.NoDirExists(t, filepath.Dir(path))
}

func TestProgramContextFile_RemovalFailureAfterPassingProgramIsGradingError(t *testing.T) {
	_, path, err := gradeWithContextHooks(t, "pass", programContextHooks{
		remove: func(func() error) error { return errors.New("injected unlink failure") },
	})
	require.ErrorContains(t, err, "injected unlink failure")
	require.ErrorContains(t, err, path)
	require.ErrorContains(t, err, "left in place")
	require.FileExists(t, path, "residue must remain locatable at the reported path")
}

func TestProgramContextFile_GuardCloseFailureIsGradingError(t *testing.T) {
	_, path, err := gradeWithContextHooks(t, "pass", programContextHooks{
		closeGuard: func(close func() error) error {
			return errors.Join(close(), errors.New("injected close failure"))
		},
	})
	require.ErrorContains(t, err, "injected close failure")
	require.ErrorContains(t, err, path)
}

func TestProgramContextFile_PartialWriteFailureCleansUpBeforeRunning(t *testing.T) {
	_, path, err := gradeWithContextHooks(t, "litter", programContextHooks{
		write: func(write func([]byte) (int, error), data []byte) (int, error) {
			n, _ := write(data[:len(data)/2])
			return n, errors.New("injected disk full")
		},
	})
	require.ErrorContains(t, err, "writing grading context")
	require.ErrorContains(t, err, "injected disk full")
	require.NoDirExists(t, filepath.Dir(path), "partial context and its directory must be removed; the program must not run")
}

func TestProgramContextFile_ShortWriteIsDetected(t *testing.T) {
	_, path, err := gradeWithContextHooks(t, "pass", programContextHooks{
		write: func(write func([]byte) (int, error), data []byte) (int, error) {
			return write(data[:1])
		},
	})
	require.ErrorContains(t, err, "short write")
	require.NoDirExists(t, filepath.Dir(path))
}

func TestProgramContextFile_PartialWriteWithRemovalFailureReportsResidue(t *testing.T) {
	_, path, err := gradeWithContextHooks(t, "pass", programContextHooks{
		write: func(write func([]byte) (int, error), data []byte) (int, error) {
			n, _ := write(data[:len(data)/2])
			return n, errors.New("injected disk full")
		},
		remove: func(func() error) error { return errors.New("injected unlink failure") },
	})
	require.ErrorContains(t, err, "injected disk full")
	require.ErrorContains(t, err, "injected unlink failure")
	require.ErrorContains(t, err, path)
	require.FileExists(t, path)
}

func TestProgramContextFile_ReplacementBeforeCleanupIsNotRemoved(t *testing.T) {
	_, path, err := gradeWithContextHooks(t, "pass", programContextHooks{
		beforeRemove: func(p string) {
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.WriteFile(p, []byte("user data"), 0o600))
		},
	})
	require.ErrorContains(t, err, "was replaced")
	require.ErrorContains(t, err, path)
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "user data", string(data), "the replacement must be preserved")
}

func TestProgramContextFile_ProgramReplacingContextFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		// The guard handle denies deletion while the program runs.
		results, report := runProgramContextHelper(t, "replace", &Context{Output: "out", WorkspaceDir: t.TempDir()})
		require.True(t, results.Passed)
		require.NotEmpty(t, report.ReplaceError)
		require.NoDirExists(t, filepath.Dir(report.Path))
		return
	}
	_, path, err := gradeWithContextHooks(t, "replace", programContextHooks{})
	require.ErrorContains(t, err, "was replaced")
	data, readErr := os.ReadFile(path)
	require.NoError(t, readErr)
	require.Equal(t, "replacement", string(data))
}

func TestProgramContextFile_ExtraEntriesAreNotDeleted(t *testing.T) {
	_, path, err := gradeWithContextHooks(t, "litter", programContextHooks{})
	require.ErrorContains(t, err, "removing grading context directory")
	require.ErrorContains(t, err, filepath.Dir(path))
	require.NoFileExists(t, path, "the verified context file is still removed")
	require.FileExists(t, filepath.Join(filepath.Dir(path), "extra.txt"))
}

func TestProgramContextFile_ParentDirectoryReplacementIsNotRemoved(t *testing.T) {
	for _, replacement := range []string{"empty directory", "regular file"} {
		t.Run(replacement, func(t *testing.T) {
			var renameErr error
			var moved string
			_, path, err := gradeWithContextHooks(t, "pass", programContextHooks{
				beforeRemove: func(p string) {
					dir := filepath.Dir(p)
					moved = dir + ".moved"
					if renameErr = os.Rename(dir, moved); renameErr != nil {
						return
					}
					t.Cleanup(func() { require.NoError(t, os.RemoveAll(moved)) })
					if replacement == "empty directory" {
						require.NoError(t, os.Mkdir(dir, 0o700))
					} else {
						require.NoError(t, os.WriteFile(dir, []byte("user data"), 0o600))
					}
				},
			})
			dir := filepath.Dir(path)
			if runtime.GOOS == "windows" {
				// The retained directory handle denies renaming the directory.
				require.Error(t, renameErr)
				require.NoError(t, err)
				require.NoDirExists(t, dir)
				return
			}
			require.NoError(t, renameErr)
			require.ErrorContains(t, err, "grading context directory "+dir+" was replaced")
			require.NoFileExists(t, filepath.Join(moved, programContextFileName), "the verified context file is still removed from the moved directory")
			info, statErr := os.Lstat(dir)
			require.NoError(t, statErr, "the replacement must be preserved")
			require.Equal(t, replacement == "empty directory", info.IsDir())
		})
	}
}

func TestProgramContextFile_PreexistingDirectoryEntryIsNeverAdopted(t *testing.T) {
	for _, existing := range []string{"empty directory", "regular file"} {
		t.Run(existing, func(t *testing.T) {
			g := newProgramContextHelperGrader(t, "pass")
			var dir string
			createdCalled := false
			g.contextHooks = programContextHooks{
				beforeCreateDir: func(d string) {
					dir = d
					t.Cleanup(func() { require.NoError(t, os.RemoveAll(d)) })
					if existing == "empty directory" {
						require.NoError(t, os.Mkdir(d, 0o700))
					} else {
						require.NoError(t, os.WriteFile(d, []byte("user data"), 0o600))
					}
				},
				created: func(string) { createdCalled = true },
			}
			_, err := g.Grade(t.Context(), &Context{Output: "out", WorkspaceDir: t.TempDir()})
			require.ErrorContains(t, err, "creating private grading context directory "+dir)
			require.False(t, createdCalled, "no context file may be written into an adopted entry")
			info, statErr := os.Lstat(dir)
			require.NoError(t, statErr, "the existing entry must be preserved")
			require.Equal(t, existing == "empty directory", info.IsDir())
			if info.IsDir() {
				entries, readErr := os.ReadDir(dir)
				require.NoError(t, readErr)
				require.Empty(t, entries)
			}
		})
	}
}

func TestProgramContextFile_HardLinkByProgramIsReportedAsResidue(t *testing.T) {
	link := filepath.Join(t.TempDir(), "kept.json")
	t.Setenv(programContextLinkEnv, link)
	_, path, err := gradeWithContextHooks(t, "link", programContextHooks{})
	if _, statErr := os.Lstat(link); statErr != nil {
		t.Skipf("platform refused the hard link while Waza held the file: %v", statErr)
	}
	require.ErrorContains(t, err, path)
	require.ErrorContains(t, err, "transcript data remain")
	require.NoFileExists(t, path, "Waza's own name is still removed")
	require.FileExists(t, link, "the other name is not Waza's to delete")
}

func TestProgramContextFile_MovedBeforeCleanupIsReportedAsResidue(t *testing.T) {
	moved := filepath.Join(t.TempDir(), "moved.json")
	_, path, err := gradeWithContextHooks(t, "pass", programContextHooks{
		beforeRemove: func(p string) { require.NoError(t, os.Rename(p, moved)) },
	})
	require.ErrorContains(t, err, path)
	require.ErrorContains(t, err, "transcript data")
	require.FileExists(t, moved, "a moved file is reported, not deleted")
	require.NoDirExists(t, filepath.Dir(path), "the now-empty owned directory is removed")
}

func TestProgramContextFile_HardLinkAddedJustBeforeDeleteIsReported(t *testing.T) {
	link := filepath.Join(t.TempDir(), "late.json")
	var contextPath string
	_, path, err := gradeWithContextHooks(t, "pass", programContextHooks{
		created: func(p string) { contextPath = p },
		remove: func(remove func() error) error {
			require.NoError(t, os.Link(contextPath, link))
			return remove()
		},
	})
	require.ErrorContains(t, err, path)
	require.ErrorContains(t, err, "transcript data")
	require.FileExists(t, link, "the other name is not Waza's to delete")
	require.NoFileExists(t, path)
}

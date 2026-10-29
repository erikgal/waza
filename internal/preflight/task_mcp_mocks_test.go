package preflight

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInspectTaskMCPMocks(t *testing.T) {
	eval := strings.ReplaceAll(validEval, "executor: mock", "executor: copilot-sdk") + `
mcp_mocks:
  - name: host
    tools:
      ask_user:
        responses: [{return: {answer: Not now}}]
`
	task := validTask + `
mcp_mocks:
  - name: host
    tools:
      ask_user:
        responses: [{return: {answer: Publish this version}}]
  - name: extra
    fixtures: missing-fixture-directory
`
	path, _ := writeSuite(t, eval, task)
	r := Inspect(path, Options{})
	require.Contains(t, r.Dependencies, Dependency{Kind: "mcp", Name: "host", Mode: "mocked", State: Verified})
	require.Contains(t, r.Dependencies, Dependency{Kind: "mcp", Name: "host", Mode: "mocked", State: Verified, TaskID: "task"})
	require.Contains(t, r.Dependencies, Dependency{Kind: "mcp", Name: "extra", Mode: "mocked", State: Invalid, TaskID: "task"})
	require.True(t, hasDiagnostic(r, "mock.mcp_task_override", Verified))
	require.True(t, hasDiagnostic(r, "mock.mcp", Invalid), "%+v", r.Diagnostics)
	for _, d := range r.Diagnostics {
		if d.Code == "mock.mcp" {
			require.Equal(t, "task", d.TaskID, "a task mock failure names its task")
		}
	}
}

func TestInspectTaskMCPMockInvalidMatcherIsTaskConfiguration(t *testing.T) {
	eval := strings.ReplaceAll(validEval, "executor: mock", "executor: copilot-sdk")
	task := validTask + `
mcp_mocks:
  - name: host
    tools:
      ask_user:
        responses: [{match_regex: {question: "("}, return: {answer: Not now}}]
`
	path, _ := writeSuite(t, eval, task)
	r := Inspect(path, Options{})
	require.True(t, hasDiagnostic(r, "task.configuration", Invalid), "%+v", r.Diagnostics)
}

func TestInspectTaskMCPMocksRequireSupportedExecutorAndVersion(t *testing.T) {
	task := validTask + `
mcp_mocks:
  - name: host
    tools:
      ask_user:
        responses: [{return: {answer: Not now}}]
`
	path, _ := writeSuite(t, validEval, task)
	r := Inspect(path, Options{})
	require.True(t, hasDiagnostic(r, "task.executor", Invalid), "%+v", r.Diagnostics)

	eval := strings.ReplaceAll(strings.ReplaceAll(validEval, "executor: mock", "executor: copilot-sdk"), `schemaVersion: "1.4"`, `schemaVersion: "1.0"`)
	path, _ = writeSuite(t, eval, task)
	r = Inspect(path, Options{})
	require.True(t, hasDiagnostic(r, "task.version", Invalid), "%+v", r.Diagnostics)
}

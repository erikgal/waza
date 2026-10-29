package graders

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/microsoft/waza/internal/models"
	"github.com/stretchr/testify/require"
)

const programContextHelperEnv = "WAZA_PROGRAM_CONTEXT_HELPER"

// TestProgramGradingContextHelper is executed as the program grader. It
// reports stdin, the context path and the context file bytes as JSON.
func TestProgramGradingContextHelper(t *testing.T) {
	mode := os.Getenv(programContextHelperEnv)
	if mode == "" {
		return
	}
	stdin, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	path, ok := os.LookupEnv(ProgramGradingContextEnv)
	report := map[string]any{"stdin": string(stdin), "path": path, "path_set": ok}
	if data, err := os.ReadFile(path); err == nil {
		report["context"] = json.RawMessage(data)
	} else {
		report["read_error"] = err.Error()
	}
	out, err := json.Marshal(report)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if mode == "fail" {
		fmt.Fprint(os.Stderr, string(out))
		os.Exit(3)
	}
	fmt.Print(string(out))
	os.Exit(0)
}

type programContextReport struct {
	Stdin     string          `json:"stdin"`
	Path      string          `json:"path"`
	PathSet   bool            `json:"path_set"`
	Context   json.RawMessage `json:"context"`
	ReadError string          `json:"read_error"`
}

func runProgramContextHelper(t *testing.T, mode string, gradingContext *Context) (*models.GraderResults, programContextReport) {
	t.Helper()
	t.Setenv(programContextHelperEnv, mode)
	exe, err := os.Executable()
	require.NoError(t, err)
	g, err := NewProgramGrader("context", models.ProgramGraderParameters{
		Command: exe,
		Args:    []string{"-test.run=^TestProgramGradingContextHelper$"},
		Timeout: 60,
	})
	require.NoError(t, err)
	results, err := g.Grade(t.Context(), gradingContext)
	require.NoError(t, err)

	raw := results.Feedback
	if mode == "fail" {
		stderr, ok := results.Details["stderr"].(string)
		require.True(t, ok)
		raw = stderr
	}
	var report programContextReport
	require.NoError(t, json.Unmarshal([]byte(raw), &report), raw)
	require.True(t, report.PathSet)
	require.Empty(t, report.ReadError)
	require.NotEmpty(t, report.Path)
	return results, report
}

func TestProgramGrader_GradingContextMatchesInlineScriptPayload(t *testing.T) {
	exitCode := 1
	gradingContext := &Context{
		Output:       "final answer",
		DurationMS:   1234,
		WorkspaceDir: t.TempDir(),
		ToolEvents: []models.ToolEvent{{
			ToolName: "dependency-request",
			Args:     map[string]any{"operation": "transient"},
			Success:  true,
		}},
		CommandInvocations: []models.CommandInvocation{{
			Command:  "az",
			Args:     []string{"account", "show"},
			ExitCode: &exitCode,
		}},
	}

	results, report := runProgramContextHelper(t, "pass", gradingContext)
	require.True(t, results.Passed)
	require.Equal(t, "final answer", report.Stdin, "stdin must remain the final output")
	require.NoFileExists(t, report.Path, "context file must be removed after the program exits")

	var programFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(report.Context, &programFields))
	require.JSONEq(t, "1", string(programFields["version"]))

	inline, err := getStdinTextForScript(gradingContext, []string{"True"})
	require.NoError(t, err)
	var inlineFields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(inline, &inlineFields))
	for _, key := range []string{"output", "outcome", "transcript", "tool_calls", "tool_events", "command_invocations", "duration_ms"} {
		require.Contains(t, programFields, key)
		require.JSONEq(t, string(inlineFields[key]), string(programFields[key]), key)
	}
	require.NotContains(t, programFields, "assertions")
	require.NotContains(t, programFields, "debug")

	var invocations []models.CommandInvocation
	require.NoError(t, json.Unmarshal(programFields["command_invocations"], &invocations))
	require.Equal(t, gradingContext.CommandInvocations, invocations)
}

func TestProgramGrader_GradingContextDistinguishesMissingAndEmptyEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		events   []models.ToolEvent
		commands []models.CommandInvocation
		want     string
	}{
		{name: "missing", want: "null"},
		{name: "empty", events: []models.ToolEvent{}, commands: []models.CommandInvocation{}, want: "[]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, report := runProgramContextHelper(t, "pass", &Context{
				Output:             "out",
				WorkspaceDir:       t.TempDir(),
				ToolEvents:         tc.events,
				CommandInvocations: tc.commands,
			})
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(report.Context, &fields))
			for _, key := range []string{"tool_events", "command_invocations"} {
				raw, ok := fields[key]
				require.True(t, ok, "%s must be present", key)
				require.Equal(t, tc.want, strings.TrimSpace(string(raw)), key)
			}
		})
	}
}

func TestProgramGrader_GradingContextRemovedAfterFailure(t *testing.T) {
	results, report := runProgramContextHelper(t, "fail", &Context{Output: "out", WorkspaceDir: t.TempDir()})
	require.False(t, results.Passed)
	require.Equal(t, 0.0, results.Score)
	require.NotEmpty(t, report.Context)
	require.NoFileExists(t, report.Path)
}

func TestProgramGrader_GradingContextOverridesInheritedValue(t *testing.T) {
	stale := t.TempDir() + string(os.PathSeparator) + "stale.json"
	t.Setenv(ProgramGradingContextEnv, stale)
	_, report := runProgramContextHelper(t, "pass", &Context{Output: "out", WorkspaceDir: t.TempDir()})
	require.NotEqual(t, stale, report.Path)
}

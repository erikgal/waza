package orchestration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/microsoft/waza/internal/config"
	"github.com/microsoft/waza/internal/execution"
	"github.com/microsoft/waza/internal/jsonrpc"
	"github.com/microsoft/waza/internal/mcpmock"
	"github.com/microsoft/waza/internal/models"
	"github.com/microsoft/waza/internal/responder"
	"github.com/stretchr/testify/require"
)

// restartingMCPEngine starts a new mock server for each turn, as the native
// runtime does after resuming a disconnected session.
type restartingMCPEngine struct {
	t       *testing.T
	mu      sync.Mutex
	configs []string
	results []string
}

func (e *restartingMCPEngine) Initialize(context.Context) error       { return nil }
func (e *restartingMCPEngine) Shutdown(context.Context) error         { return nil }
func (e *restartingMCPEngine) SessionUsage(string) *models.UsageStats { return nil }

func (e *restartingMCPEngine) Execute(ctx context.Context, req *execution.ExecutionRequest) (*execution.ExecutionResponse, error) {
	stdio, ok := req.MCPServers["counter"].(copilot.MCPStdioServerConfig)
	require.True(e.t, ok)
	require.Len(e.t, stdio.Args, 3)
	require.Equal(e.t, "--attempt-config-file", stdio.Args[1])
	data, err := os.ReadFile(stdio.Args[2])
	require.NoError(e.t, err)
	var cfg mcpmock.Config
	require.NoError(e.t, json.Unmarshal(data, &cfg))
	params, err := json.Marshal(map[string]any{"name": "read", "arguments": map[string]any{"id": "item"}})
	require.NoError(e.t, err)
	resp := mcpmock.NewServer(&cfg, nil).HandleRequest(ctx, &jsonrpc.Request{Method: "tools/call", Params: params, ID: json.RawMessage(`1`)})
	require.Nil(e.t, resp.Error)
	result, err := json.Marshal(resp.Result)
	require.NoError(e.t, err)
	e.mu.Lock()
	e.configs = append(e.configs, stdio.Args[2])
	e.results = append(e.results, string(result))
	e.mu.Unlock()
	return &execution.ExecutionResponse{FinalOutput: string(result), SessionID: "session", WorkspaceDir: "workspace", Success: true}, nil
}

func newMCPLifetimeRunner(t *testing.T) (*EvalRunner, *restartingMCPEngine, string) {
	t.Helper()
	tmp := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, tmp)
	}
	spec := &models.EvalSpec{
		SchemaVersion: models.ScenarioSchemaVersion,
		SpecIdentity:  models.SpecIdentity{Name: "mcp-lifetime"},
		Config:        models.Config{TrialsPerTask: 1, TimeoutSec: 30},
		MCPMocks: []models.MCPMockConfig{{Name: "counter", Tools: map[string]models.MCPMockTool{"read": {Responses: []models.MCPMockResponse{
			{Match: map[string]any{"id": "item"}, Sequence: []models.MCPMockStep{
				{Return: map[string]any{"ordinal": 1}}, {Return: map[string]any{"ordinal": 2}},
			}},
			{Match: map[string]any{"id": "item"}, Return: map[string]any{"fallback": true}},
		}}}}},
	}
	engine := &restartingMCPEngine{t: t}
	return NewEvalRunner(config.NewEvalConfig(spec, config.WithSpecDir(tmp)), engine, WithSkipGraders()), engine, tmp
}

func requireMCPLifetimeResults(t *testing.T, engine *restartingMCPEngine, tmp string) {
	t.Helper()
	require.Len(t, engine.results, 6)
	for attempt := range 2 {
		results := engine.results[attempt*3 : attempt*3+3]
		require.Contains(t, results[0], `{\"ordinal\":1}`)
		require.Contains(t, results[1], `{\"ordinal\":2}`)
		require.Contains(t, results[2], "response sequence exhausted")
		require.NotContains(t, results[2], "fallback", "an exhausted first match must not fall through")
		configs := engine.configs[attempt*3 : attempt*3+3]
		require.Equal(t, configs[0], configs[1])
		require.Equal(t, configs[0], configs[2])
		require.NoFileExists(t, configs[0], "the attempt removes its retained configuration")
	}
	require.NotEqual(t, engine.configs[0], engine.configs[3], "each attempt owns separate state")
	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	for _, entry := range entries {
		require.False(t, strings.HasPrefix(entry.Name(), "waza-mcp-"), "leftover MCP mock state %s", filepath.Join(tmp, entry.Name()))
	}
}

func TestExecuteRunMCPMockSequenceSpansFollowUpsAndResetsPerAttempt(t *testing.T) {
	runner, engine, tmp := newMCPLifetimeRunner(t)
	tc := &models.TestCase{TestID: "follow-up", Stimulus: models.TaskStimulus{
		Message: "read", FollowUps: []string{"read again", "read a third time"},
	}}

	for attempt := 1; attempt <= 2; attempt++ {
		run := runner.executeRun(context.Background(), tc, attempt)
		require.Equal(t, models.StatusSkipped, run.Status, run.ErrorMsg)
	}
	requireMCPLifetimeResults(t, engine, tmp)
}

func TestExecuteRunMCPMockSequenceSpansResponderTurnsAndResetsPerAttempt(t *testing.T) {
	runner, engine, tmp := newMCPLifetimeRunner(t)
	runner.newClassifier = func(models.ResponderConfig, string) responderClassifier {
		return &scriptedClassifier{decisions: []responder.Decision{
			{Kind: responder.DecisionReply, Answer: "read again"},
			{Kind: responder.DecisionReply, Answer: "read a third time"},
			{Kind: responder.DecisionStop},
		}}
	}
	tc := &models.TestCase{TestID: "responder", Stimulus: models.TaskStimulus{
		Message: "read", Responder: &models.ResponderConfig{Instructions: "ask twice", MaxFollowups: 3},
	}}

	for attempt := 1; attempt <= 2; attempt++ {
		run := runner.executeRun(context.Background(), tc, attempt)
		require.Equal(t, models.StatusSkipped, run.Status, run.ErrorMsg)
		require.Equal(t, models.ResponderOutcomeStopped, run.Responder.Outcome)
	}
	requireMCPLifetimeResults(t, engine, tmp)
}

func TestExecuteRunMCPMockSetupFailureRemovesAttemptState(t *testing.T) {
	runner, engine, tmp := newMCPLifetimeRunner(t)
	runner.cfg.Spec().MCPMocks = append(runner.cfg.Spec().MCPMocks, models.MCPMockConfig{Name: "broken",
		Tools: map[string]models.MCPMockTool{"read": {Responses: []models.MCPMockResponse{{Return: make(chan int)}}}}})

	run := runner.executeRun(context.Background(), &models.TestCase{TestID: "setup", Stimulus: models.TaskStimulus{Message: "read"}}, 1)
	require.Equal(t, models.StatusError, run.Status)
	require.Contains(t, run.ErrorMsg, `configure MCP mock "broken"`)
	require.Empty(t, engine.results)
	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	require.Empty(t, entries)
}

package copilotconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/microsoft/waza/internal/mcpmock"
	"github.com/microsoft/waza/internal/models"
	"github.com/stretchr/testify/require"
)

func setTempRoot(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, dir)
	}
}

func sequenceMock(name string) models.MCPMockConfig {
	return models.MCPMockConfig{Name: name, Tools: map[string]models.MCPMockTool{"read": {Responses: []models.MCPMockResponse{{
		Match:    map[string]any{"id": "item"},
		Sequence: []models.MCPMockStep{{Return: map[string]any{"ordinal": 1}}, {Return: map[string]any{"ordinal": 2}}},
	}}}}}
}

func attemptConfig(t *testing.T, server copilot.MCPServerConfig) (mcpmock.Config, string) {
	t.Helper()
	stdio, ok := server.(copilot.MCPStdioServerConfig)
	require.True(t, ok)
	require.Len(t, stdio.Args, 3)
	require.Equal(t, []string{"__mcp-mock", "--attempt-config-file"}, stdio.Args[:2])
	data, err := os.ReadFile(stdio.Args[2])
	require.NoError(t, err)
	var cfg mcpmock.Config
	require.NoError(t, json.Unmarshal(data, &cfg))
	return cfg, stdio.Args[2]
}

func TestResolveMCPServersForAttemptOwnsRetainedState(t *testing.T) {
	tmp := t.TempDir()
	setTempRoot(t, tmp)
	servers, attempt, err := ResolveMCPServersForAttempt(map[string]any{
		"live": map[string]any{"type": "stdio", "command": "server"},
	}, []models.MCPMockConfig{sequenceMock("counter")}, tmp, models.ScenarioSchemaVersion, nil)
	require.NoError(t, err)
	require.NotNil(t, attempt)
	require.IsType(t, copilot.MCPStdioServerConfig{}, servers["live"])
	cfg, configFile := attemptConfig(t, servers["counter"])
	require.True(t, filepath.IsAbs(cfg.StateDir))
	require.DirExists(t, cfg.StateDir)
	require.FileExists(t, configFile, "restarted servers need the retained configuration")
	rel, err := filepath.Rel(tmp, configFile)
	require.NoError(t, err)
	require.True(t, filepath.IsLocal(rel))

	_, other, err := ResolveMCPServersForAttempt(nil, []models.MCPMockConfig{sequenceMock("counter")}, tmp, models.ScenarioSchemaVersion, nil)
	require.NoError(t, err)
	otherServers, otherAttempt, err := ResolveMCPServersForAttempt(nil, []models.MCPMockConfig{sequenceMock("counter")}, tmp, models.ScenarioSchemaVersion, nil)
	require.NoError(t, err)
	otherCfg, _ := attemptConfig(t, otherServers["counter"])
	require.NotEqual(t, cfg.StateDir, otherCfg.StateDir, "attempts must not share sequence state")

	require.NoError(t, attempt.Close())
	require.NoError(t, attempt.Close())
	require.NoFileExists(t, configFile)
	require.NoDirExists(t, cfg.StateDir)
	require.DirExists(t, otherCfg.StateDir, "closing one attempt must not remove another")
	require.NoError(t, other.Close())
	require.NoError(t, otherAttempt.Close())
	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestResolveMCPServersForAttemptWithoutMocksOwnsNothing(t *testing.T) {
	tmp := t.TempDir()
	setTempRoot(t, tmp)
	servers, attempt, err := ResolveMCPServersForAttempt(map[string]any{
		"live": map[string]any{"type": "stdio", "command": "server"},
	}, nil, tmp, models.ScenarioSchemaVersion, nil)
	require.NoError(t, err)
	require.Nil(t, attempt)
	require.NoError(t, attempt.Close())
	require.Contains(t, servers, "live")
	entries, err := os.ReadDir(tmp)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestResolveMCPServersForAttemptRemovesPartialSetup(t *testing.T) {
	tmp := t.TempDir()
	setTempRoot(t, tmp)
	servers, attempt, err := ResolveMCPServersForAttempt(nil, []models.MCPMockConfig{
		sequenceMock("first"),
		{Name: "second", Tools: map[string]models.MCPMockTool{"request": {Responses: []models.MCPMockResponse{{Return: make(chan int)}}}}},
	}, tmp, models.ScenarioSchemaVersion, nil)
	require.ErrorContains(t, err, `configure MCP mock "second"`)
	require.Nil(t, servers)
	require.Nil(t, attempt)
	entries, readErr := os.ReadDir(tmp)
	require.NoError(t, readErr)
	require.Empty(t, entries, "a failed attempt setup must remove only its own state")
}

func TestResolveMCPServersForAttemptPreservesAdmission(t *testing.T) {
	tmp := t.TempDir()
	setTempRoot(t, tmp)
	_, attempt, err := ResolveMCPServersForAttempt(nil, []models.MCPMockConfig{sequenceMock("counter")}, tmp, "1.4", nil)
	require.ErrorContains(t, err, "requires explicit enclosing scenario schemaVersion")
	require.Nil(t, attempt)
	entries, readErr := os.ReadDir(tmp)
	require.NoError(t, readErr)
	require.Empty(t, entries)
}

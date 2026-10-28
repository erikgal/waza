package mcpmock

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func attemptStateConfig(t *testing.T) *Config {
	t.Helper()
	cfg := &Config{Name: "counter", Tools: map[string]Tool{
		"read": {Responses: []Response{
			{Match: map[string]any{"id": "item"}, Sequence: []ResponseStep{
				{Return: map[string]any{"ordinal": 1}},
				{Return: map[string]any{"ordinal": 2}},
			}},
			{Match: map[string]any{"id": "item"}, Return: map[string]any{"fallback": true}},
		}},
		"write": {Responses: []Response{{Sequence: []ResponseStep{{Return: "written"}}}}},
	}}
	require.NoError(t, PrepareSequenceState(cfg, filepath.Join(t.TempDir(), "state")))
	return cfg
}

func TestAttemptSequenceStateSurvivesServerRestarts(t *testing.T) {
	cfg := attemptStateConfig(t)
	args := map[string]any{"id": "item"}

	requireToolJSON(t, NewServer(cfg, nil), "read", args, `"ordinal":1`)
	requireToolJSON(t, NewServer(cfg, nil), "read", args, `"ordinal":2`)
	requireToolError(t, NewServer(cfg, nil), "read", args, "response sequence exhausted")
	requireToolJSON(t, NewServer(cfg, nil), "write", map[string]any{}, `"written"`)

	fresh := attemptStateConfig(t)
	requireToolJSON(t, NewServer(fresh, nil), "read", args, `"ordinal":1`)
}

func TestAttemptSequenceStateKeepsInMemoryControl(t *testing.T) {
	cfg := attemptStateConfig(t)
	legacy := *cfg
	legacy.StateDir = ""
	args := map[string]any{"id": "item"}

	requireToolJSON(t, NewServer(&legacy, nil), "read", args, `"ordinal":1`)
	requireToolJSON(t, NewServer(&legacy, nil), "read", args, `"ordinal":1`)
}

func TestAttemptSequenceStateReservesDistinctStepsAcrossServers(t *testing.T) {
	cfg := attemptStateConfig(t)
	results := make(chan string, 2)
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, err := NewServer(cfg, nil).call(context.Background(), "read", map[string]any{"id": "item"})
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var got []string
	for result := range results {
		got = append(got, result)
	}
	require.ElementsMatch(t, []string{`{"ordinal":1}`, `{"ordinal":2}`}, got)
}

func TestAttemptSequenceStateCancellationConsumesStartedStep(t *testing.T) {
	cfg := &Config{Name: "inventory", Tools: map[string]Tool{
		"read": {Responses: []Response{{Sequence: []ResponseStep{
			{Error: "late failure", DelayMS: 1000},
			{Return: "recovered"},
		}}}},
	}}
	require.NoError(t, PrepareSequenceState(cfg, filepath.Join(t.TempDir(), "state")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewServer(cfg, nil).call(ctx, "read", map[string]any{})
	require.ErrorContains(t, err, "canceled")
	requireToolJSON(t, NewServer(cfg, nil), "read", map[string]any{}, `"recovered"`)
}

func TestAttemptSequenceStateReportsUnavailableState(t *testing.T) {
	cfg := attemptStateConfig(t)
	require.NoError(t, os.RemoveAll(cfg.StateDir))
	requireToolError(t, NewServer(cfg, nil), "read", map[string]any{"id": "item"}, "reserving fault sequence step")
}

func TestPrepareSequenceStateRejectsUnsafeRoots(t *testing.T) {
	cfg := &Config{Name: "counter", Tools: map[string]Tool{}}
	require.ErrorContains(t, PrepareSequenceState(cfg, "relative"), "absolute private path")
	existing := t.TempDir()
	require.ErrorContains(t, PrepareSequenceState(cfg, existing), "create mcp mock sequence state")
	require.Empty(t, cfg.StateDir)
}

package graders

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/microsoft/waza/internal/models"
	"github.com/microsoft/waza/internal/projectconfig"
)

// defaultProgramTimeoutSeconds is the default timeout for program graders when none is specified.
const defaultProgramTimeoutSeconds = 30

// ProgramGradingContextEnv names the environment variable holding the path of
// the grading context JSON file passed to program graders.
const ProgramGradingContextEnv = "WAZA_GRADING_CONTEXT"

// ProgramGradingContextVersion is the version of the program grading context
// JSON. Additive fields keep the version; incompatible changes increment it.
const ProgramGradingContextVersion = 1

// programGrader runs an external program/script to grade agent output.
// The agent output is passed via stdin, the workspace directory is available
// as WAZA_WORKSPACE_DIR, and WAZA_GRADING_CONTEXT names a temporary JSON file
// with the run's grading context (removed after the program exits).
// Exit code 0 = pass (1.0), non-zero = fail (0.0).
type programGrader struct {
	name    string
	command string
	args    []string
	timeout time.Duration
}

// NewProgramGrader creates a [programGrader] that runs an external command to grade output.
func NewProgramGrader(name string, args models.ProgramGraderParameters) (*programGrader, error) {
	if err := validateProgramParameters(name, args); err != nil {
		return nil, err
	}

	timeout := args.Timeout
	if timeout <= 0 {
		// Per-grader timeout not specified; use project config default
		cfg, err := projectconfig.Load(".")
		if err == nil && cfg != nil && cfg.Graders.ProgramTimeout > 0 {
			timeout = cfg.Graders.ProgramTimeout
		} else {
			timeout = defaultProgramTimeoutSeconds
		}
	}

	return &programGrader{
		name:    name,
		command: args.Command,
		args:    args.Args,
		timeout: time.Duration(timeout) * time.Second,
	}, nil
}

func validateProgramParameters(name string, args models.ProgramGraderParameters) error {
	if args.Command == "" {
		return fmt.Errorf("program grader '%s' must have a 'command'", name)
	}
	return nil
}

func (pg *programGrader) Name() string            { return pg.name }
func (pg *programGrader) Kind() models.GraderKind { return models.GraderKindProgram }

func (pg *programGrader) Grade(ctx context.Context, gradingContext *Context) (*models.GraderResults, error) {
	return measureTime(func() (*models.GraderResults, error) {
		contextPath, err := writeProgramGradingContext(gradingContext)
		if err != nil {
			return nil, fmt.Errorf("program grader '%s': writing grading context: %w", pg.name, err)
		}

		results := pg.run(ctx, gradingContext, contextPath)

		// The file can contain transcript and tool evidence; report residue
		// instead of claiming a clean result when it cannot be removed.
		if err := os.Remove(contextPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("program grader '%s': removing grading context file %s: %w", pg.name, contextPath, err)
		}
		return results, nil
	})
}

// writeProgramGradingContext writes the versioned grading context JSON to an
// exclusively created, owner-only temporary file and returns its path.
func writeProgramGradingContext(gradingContext *Context) (string, error) {
	payload := struct {
		Version int `json:"version"`
		gradingContextPayload
	}{
		Version:               ProgramGradingContextVersion,
		gradingContextPayload: newGradingContextPayload(gradingContext),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	f, err := os.CreateTemp("", "waza-grading-context-*.json")
	if err != nil {
		return "", err
	}
	path := f.Name()
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		if rmErr := os.Remove(path); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			return "", errors.Join(err, fmt.Errorf("removing partial grading context file %s: %w", path, rmErr))
		}
		return "", err
	}
	return path, nil
}

func (pg *programGrader) run(ctx context.Context, gradingContext *Context, contextPath string) *models.GraderResults {
	timeoutCtx, cancel := context.WithTimeout(ctx, pg.timeout)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, pg.command, pg.args...)

	// Pass agent output via stdin
	cmd.Stdin = strings.NewReader(gradingContext.Output)

	cmd.Env = append(cmd.Environ(),
		fmt.Sprintf("WAZA_WORKSPACE_DIR=%s", gradingContext.WorkspaceDir),
		fmt.Sprintf("%s=%s", ProgramGradingContextEnv, contextPath),
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	notes := strings.TrimSpace(stdout.String())
	errOutput := strings.TrimSpace(stderr.String())

	if err != nil {
		feedback := fmt.Sprintf("Program exited with error: %v", err)
		if errOutput != "" {
			feedback = fmt.Sprintf("%s; stderr: %s", feedback, errOutput)
		}

		return &models.GraderResults{
			Name:     pg.name,
			Type:     models.GraderKindProgram,
			Score:    0.0,
			Passed:   false,
			Feedback: feedback,
			Details: map[string]any{
				"command":       pg.command,
				"args":          pg.args,
				"exit_error":    err.Error(),
				"stdout":        notes,
				"stderr":        errOutput,
				"workspace_dir": gradingContext.WorkspaceDir,
			},
		}
	}

	feedback := "Program exited successfully"
	if notes != "" {
		feedback = notes
	}

	return &models.GraderResults{
		Name:     pg.name,
		Type:     models.GraderKindProgram,
		Score:    1.0,
		Passed:   true,
		Feedback: feedback,
		Details: map[string]any{
			"command":       pg.command,
			"args":          pg.args,
			"stdout":        notes,
			"stderr":        errOutput,
			"workspace_dir": gradingContext.WorkspaceDir,
		},
	}
}

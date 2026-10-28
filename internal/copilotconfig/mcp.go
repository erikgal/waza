package copilotconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/go-viper/mapstructure/v2"
	"github.com/microsoft/waza/internal/mcpmock"
	"github.com/microsoft/waza/internal/models"
)

// ConvertMCPServers converts eval YAML mcp_servers entries into Copilot SDK MCP
// configs. Invalid entries are skipped after emitting a warning through warnf.
func ConvertMCPServers(serverConfigs map[string]any, warnf func(string, ...any)) map[string]copilot.MCPServerConfig {
	return ConvertMCPServersWithMocks(serverConfigs, nil, "", warnf)
}

// ConvertMCPServersWithMocks converts eval YAML mcp_servers entries and
// mcp_mocks entries into Copilot SDK MCP configs. Mock servers are exposed as
// hermetic stdio MCP servers backed by this waza binary.
func ConvertMCPServersWithMocks(serverConfigs map[string]any, mocks []models.MCPMockConfig, baseDir string, warnf func(string, ...any)) map[string]copilot.MCPServerConfig {
	return ConvertMCPServersWithVersion(serverConfigs, mocks, baseDir, "", warnf)
}

// ConvertMCPServersWithVersion permits finite mock responses only when the
// enclosing eval schema version explicitly supports them.
func ConvertMCPServersWithVersion(serverConfigs map[string]any, mocks []models.MCPMockConfig, baseDir, version string, warnf func(string, ...any)) map[string]copilot.MCPServerConfig {
	servers, err := convertMCPServers(serverConfigs, mocks, baseDir, version, warnf, false, nil)
	if err != nil && warnf != nil {
		warnf("Warning: MCP configuration failed: %v\n", err)
	}
	return servers
}

// ResolveMCPServersWithVersion fails execution admission rather than dropping a
// declared mock. The conversion-only adapters retain their legacy warning policy.
func ResolveMCPServersWithVersion(serverConfigs map[string]any, mocks []models.MCPMockConfig, baseDir, version string, warnf func(string, ...any)) (map[string]copilot.MCPServerConfig, error) {
	return convertMCPServers(serverConfigs, mocks, baseDir, version, warnf, true, nil)
}

// MCPMockAttempt owns persistent mock configuration and finite sequence state
// shared by every actor turn in one task attempt.
type MCPMockAttempt struct {
	dir string
}

// ResolveMCPServersForAttempt fails admission like ResolveMCPServersWithVersion.
// Mock servers may restart between turns without resetting finite sequences.
// The caller must Close the returned attempt after the final turn.
func ResolveMCPServersForAttempt(serverConfigs map[string]any, mocks []models.MCPMockConfig, baseDir, version string, warnf func(string, ...any)) (map[string]copilot.MCPServerConfig, *MCPMockAttempt, error) {
	attempt := &MCPMockAttempt{}
	servers, err := convertMCPServers(serverConfigs, mocks, baseDir, version, warnf, true, attempt.serverConfig)
	if err != nil {
		return nil, nil, errors.Join(err, attempt.Close())
	}
	if attempt.dir == "" {
		return servers, nil, nil
	}
	return servers, attempt, nil
}

// Close removes this attempt's configuration and sequence state. It is safe on
// nil and repeated calls.
func (a *MCPMockAttempt) Close() error {
	if a == nil || a.dir == "" {
		return nil
	}
	dir := a.dir
	a.dir = ""
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove MCP mock attempt state %s: %w", dir, err)
	}
	return nil
}

func (a *MCPMockAttempt) serverConfig(cfg mcpmock.Config) (copilot.MCPStdioServerConfig, error) {
	if a.dir == "" {
		dir, err := os.MkdirTemp("", "waza-mcp-attempt-*")
		if err != nil {
			return copilot.MCPStdioServerConfig{}, fmt.Errorf("create MCP mock attempt directory: %w", err)
		}
		a.dir = dir
	}
	mockDir, err := os.MkdirTemp(a.dir, "mock-*")
	if err != nil {
		return copilot.MCPStdioServerConfig{}, fmt.Errorf("create MCP mock %q attempt directory: %w", cfg.Name, err)
	}
	if err := mcpmock.PrepareSequenceState(&cfg, filepath.Join(mockDir, "state")); err != nil {
		return copilot.MCPStdioServerConfig{}, err
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return copilot.MCPStdioServerConfig{}, fmt.Errorf("marshal mock config: %w", err)
	}
	configFile := filepath.Join(mockDir, "config.json")
	if err := writeExclusiveFile(configFile, data); err != nil {
		return copilot.MCPStdioServerConfig{}, fmt.Errorf("write mock config file: %w", err)
	}
	return mockStdioConfig("--attempt-config-file", configFile)
}

func writeExclusiveFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	return file.Close()
}

func convertMCPServers(serverConfigs map[string]any, mocks []models.MCPMockConfig, baseDir, version string, warnf func(string, ...any), strict bool, materialize func(mcpmock.Config) (copilot.MCPStdioServerConfig, error)) (map[string]copilot.MCPServerConfig, error) {
	ownedState := materialize != nil
	if materialize == nil {
		materialize = mockServerConfig
	}
	if warnf == nil {
		warnf = func(string, ...any) {}
	}
	if len(serverConfigs) == 0 {
		if len(mocks) == 0 {
			return nil, nil
		}
	}

	result := make(map[string]copilot.MCPServerConfig, len(serverConfigs)+len(mocks))
	for name, cfg := range serverConfigs {
		cfgMap, ok := cfg.(map[string]any)
		if !ok {
			warnf("Warning: mcp_server %q config is not a map, skipping\n", name)
			continue
		}

		serverType, _ := cfgMap["type"].(string)
		switch strings.ToLower(serverType) {
		case "", "stdio":
			stdio, err := DecodeMCPStdioServer(cfgMap)
			if err != nil {
				warnf("Warning: mcp_server %q stdio config is invalid: %v, skipping\n", name, err)
				continue
			}
			result[name] = stdio
		case "http", "sse":
			http, err := DecodeMCPHTTPServer(cfgMap)
			if err != nil {
				warnf("Warning: mcp_server %q http config is invalid: %v, skipping\n", name, err)
				continue
			}
			result[name] = http
		default:
			warnf("Warning: mcp_server %q has unsupported type %q, skipping\n", name, serverType)
		}
	}

	resolved := make([]mcpmock.Config, 0, len(mocks))
	for _, mock := range mocks {
		mockName := strings.TrimSpace(mock.Name)
		if mockName != "" {
			delete(result, mockName)
		}
		cfg, err := mcpmock.FromEvalConfigWithVersion(mock, baseDir, version)
		if err != nil {
			if strict {
				return nil, fmt.Errorf("resolve MCP mock %q: %w", mock.Name, err)
			}
			warnf("Warning: mcp_mock %q config is invalid: %v, skipping\n", mock.Name, err)
			continue
		}
		resolved = append(resolved, *cfg)
	}
	var createdPaths []string
	for _, cfg := range resolved {
		stdio, err := materialize(cfg)
		if err != nil {
			if strict && ownedState {
				return nil, fmt.Errorf("configure MCP mock %q: %w", cfg.Name, err)
			}
			if strict {
				return nil, fmt.Errorf("configure MCP mock %q (earlier temporary config files: %v): %w", cfg.Name, createdPaths, err)
			}
			warnf("Warning: mcp_mock %q could not be configured: %v, skipping\n", cfg.Name, err)
			continue
		}
		createdPaths = append(createdPaths, stdio.Args[2])
		result[cfg.Name] = stdio
	}

	if len(result) == 0 {
		return nil, nil
	}
	return result, nil
}

// DecodeMCPStdioServer performs the runtime's local decoding without starting a server.
func DecodeMCPStdioServer(input map[string]any) (copilot.MCPStdioServerConfig, error) {
	var server copilot.MCPStdioServerConfig
	if err := decode(input, &server); err != nil {
		return server, err
	}
	defaultMCPTools(input, &server.Tools)
	return server, nil
}

// DecodeMCPHTTPServer performs the runtime's local decoding without contacting a server.
func DecodeMCPHTTPServer(input map[string]any) (copilot.MCPHTTPServerConfig, error) {
	var server copilot.MCPHTTPServerConfig
	if err := decode(input, &server); err != nil {
		return server, err
	}
	defaultMCPTools(input, &server.Tools)
	return server, nil
}

func defaultMCPTools(cfgMap map[string]any, tools *[]string) {
	if value, ok := cfgMap["tools"]; !ok || value == nil {
		*tools = []string{"*"}
	}
}

func mockServerConfig(cfg mcpmock.Config) (copilot.MCPStdioServerConfig, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return copilot.MCPStdioServerConfig{}, fmt.Errorf("marshal mock config: %w", err)
	}
	configFile, err := writeMockConfigFile(cfg.Name, data)
	if err != nil {
		return copilot.MCPStdioServerConfig{}, err
	}
	stdio, err := mockStdioConfig("--config-file", configFile)
	if err != nil {
		return copilot.MCPStdioServerConfig{}, errors.Join(err, os.Remove(configFile))
	}
	return stdio, nil
}

func mockStdioConfig(flag, configFile string) (copilot.MCPStdioServerConfig, error) {
	exe, err := os.Executable()
	if err != nil {
		return copilot.MCPStdioServerConfig{}, fmt.Errorf("resolve waza executable: %w", err)
	}
	return copilot.MCPStdioServerConfig{
		Command: exe,
		Args:    []string{"__mcp-mock", flag, configFile},
		// The bundled CLI rejects mock servers when the allowlist is omitted.
		Tools: []string{"*"},
		Env: map[string]string{
			"WAZA_NO_UPDATE_CHECK": "1",
		},
	}, nil
}

func writeMockConfigFile(name string, data []byte) (string, error) {
	safeName := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_':
			return r
		default:
			return '-'
		}
	}, name)
	if safeName == "" {
		safeName = "mock"
	}
	path := filepath.Join(os.TempDir(), fmt.Sprintf("waza-mcp-mock-%s-*.json", safeName))
	file, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path))
	if err != nil {
		return "", fmt.Errorf("create mock config file: %w", err)
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("secure mock config file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("write mock config file: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(file.Name())
		return "", fmt.Errorf("close mock config file: %w", err)
	}
	return file.Name(), nil
}

func decode(input map[string]any, output any) error {
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           output,
		TagName:          "json",
		WeaklyTypedInput: true,
	})
	if err != nil {
		return fmt.Errorf("create decoder: %w", err)
	}
	if err := decoder.Decode(input); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

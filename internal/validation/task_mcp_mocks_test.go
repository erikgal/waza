package validation

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const taskWithMCPMocks = `id: publish
name: Publish
inputs:
  prompt: Publish it.
mcp_mocks:
  - name: host
    tools:
      ask_user:
        responses:
          - match_regex: {choices: "Publish this version"}
            return: {answer: Publish this version, wasFreeform: false}
          - match_schema: {}
            sequence:
              - return: {answer: Not now}
`

func TestValidateTaskBytes_MCPMocks(t *testing.T) {
	require.Empty(t, ValidateTaskBytes([]byte(taskWithMCPMocks)))
	for name, task := range map[string]string{
		"empty list":       "id: t\nname: T\ninputs: {prompt: hi}\nmcp_mocks: []\n",
		"missing name":     "id: t\nname: T\ninputs: {prompt: hi}\nmcp_mocks: [{tools: {a: {responses: [{return: 1}]}}}]\n",
		"no tools":         "id: t\nname: T\ninputs: {prompt: hi}\nmcp_mocks: [{name: host}]\n",
		"unknown field":    "id: t\nname: T\ninputs: {prompt: hi}\nmcp_mocks: [{name: host, command: x, tools: {a: {responses: [{return: 1}]}}}]\n",
		"return and error": "id: t\nname: T\ninputs: {prompt: hi}\nmcp_mocks: [{name: host, tools: {a: {responses: [{return: 1, sequence: [{error: x}]}]}}}]\n",
	} {
		t.Run(name, func(t *testing.T) {
			require.NotEmpty(t, ValidateTaskBytes([]byte(task)))
		})
	}
	require.NotEmpty(t, ValidateTaskBytes([]byte(strings.Replace(taskWithMCPMocks, "mcp_mocks:", "mcp_mock:", 1))), "misspelled field is still rejected")
}

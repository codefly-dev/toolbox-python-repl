package pythonrepl_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	toolboxv0 "github.com/codefly-dev/core/generated/go/codefly/services/toolbox/v0"
	pythonrepl "github.com/codefly-dev/toolbox-python-repl"
)

// Metadata-only tests — don't need python3 on PATH. The actual REPL
// behavior is exercised in server_python_test.go behind the
// `python_required` build tag.

func TestListToolSummaries_AdvertisesEvalAndReset(t *testing.T) {
	s := pythonrepl.New("test", "")
	defer s.Close()

	resp, err := s.ListToolSummaries(context.Background(), &toolboxv0.ListToolSummariesRequest{})
	require.NoError(t, err)

	names := make([]string, 0, len(resp.Tools))
	for _, tt := range resp.Tools {
		names = append(names, tt.Name)
	}
	require.Contains(t, names, "python.repl.eval")
	require.Contains(t, names, "python.repl.reset")
}

func TestDescribeTool_ReturnsExamplesForEval(t *testing.T) {
	s := pythonrepl.New("test", "")
	defer s.Close()

	resp, err := s.DescribeTool(context.Background(), &toolboxv0.DescribeToolRequest{Name: "python.repl.eval"})
	require.NoError(t, err)
	require.Empty(t, resp.Error)
	require.NotNil(t, resp.Tool)
	require.NotEmpty(t, resp.Tool.Examples,
		"eval must ship examples — that's the whole point of the two-phase API")

	descLower := strings.ToLower(resp.Tool.Description)
	require.True(t,
		strings.Contains(descLower, "persist") || strings.Contains(descLower, "global"),
		"description should explain state persistence — the load-bearing property")
}

func TestDescribeTool_UnknownToolReturnsError(t *testing.T) {
	s := pythonrepl.New("test", "")
	defer s.Close()

	resp, err := s.DescribeTool(context.Background(), &toolboxv0.DescribeToolRequest{Name: "nonexistent"})
	require.NoError(t, err)
	require.Nil(t, resp.Tool)
	require.Contains(t, resp.Error, "nonexistent")
}

func TestIdentity_NamePythonRepl(t *testing.T) {
	s := pythonrepl.New("test", "")
	defer s.Close()

	id, err := s.Identity(context.Background(), &toolboxv0.IdentityRequest{})
	require.NoError(t, err)
	require.Equal(t, "python-repl", id.Name)
	require.Equal(t, "test", id.Version)
	require.Empty(t, id.CanonicalFor, "REPL is not a binary canonical")
}

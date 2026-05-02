//go:build python_required

// REPL behavior tests — exercise the persistent python subprocess.
// Gated by `python_required` so a `go test ./...` on a machine
// without python3 doesn't fail loud (per the no-t.Skip rule).
//
// Run locally:  go test -tags=python_required ./...
// In CI:        the workflow always passes -tags=python_required
//               and ensures python3 is provisioned.

package pythonrepl_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/structpb"

	toolboxv0 "github.com/codefly-dev/core/generated/go/codefly/services/toolbox/v0"
	pythonrepl "github.com/codefly-dev/toolbox-python-repl"
)

func newServer(t *testing.T) *pythonrepl.Server {
	t.Helper()
	s := pythonrepl.New("test", "")
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func eval(t *testing.T, s *pythonrepl.Server, code string) (stdout, stderr string, epoch float64) {
	t.Helper()
	args, err := structpb.NewStruct(map[string]any{"code": code})
	require.NoError(t, err)
	resp, err := s.CallTool(context.Background(), &toolboxv0.CallToolRequest{
		Name:      "python.repl.eval",
		Arguments: args,
	})
	require.NoError(t, err)
	require.Empty(t, resp.Error, "eval returned in-band error")
	require.NotEmpty(t, resp.Content, "eval should return Content")
	body := resp.Content[0].GetStructured().AsMap()
	stdout, _ = body["stdout"].(string)
	stderr, _ = body["stderr"].(string)
	epoch, _ = body["session_epoch"].(float64)
	return stdout, stderr, epoch
}

// TestEval_VariablesPersistAcrossCalls is the load-bearing test for
// the whole plugin. If this passes, the persistent-subprocess design
// works end-to-end: define a variable, see it on a subsequent call.
func TestEval_VariablesPersistAcrossCalls(t *testing.T) {
	s := newServer(t)

	_, _, epoch1 := eval(t, s, "x = 42")
	stdout, _, epoch2 := eval(t, s, "print(x)")

	require.Equal(t, "42\n", stdout, "x defined in the first call must be visible in the second")
	require.Equal(t, epoch1, epoch2, "no restart in between → same epoch")
}

func TestEval_ImportsPersist(t *testing.T) {
	s := newServer(t)

	eval(t, s, "import json")
	stdout, _, _ := eval(t, s, "print(json.dumps([1, 2, 3]))")
	require.Equal(t, "[1, 2, 3]\n", stdout)
}

func TestEval_ExpressionReprIsPrinted(t *testing.T) {
	s := newServer(t)

	stdout, _, _ := eval(t, s, "1 + 2")
	require.Equal(t, "3\n", stdout, "bare expression should be repr'd to stdout")
}

func TestEval_ExceptionDoesNotKillSession(t *testing.T) {
	s := newServer(t)

	// Set state, then trigger an exception, then verify state survives.
	eval(t, s, "marker = 'alive'")

	_, stderr, _ := eval(t, s, "1/0")
	require.Contains(t, stderr, "ZeroDivisionError",
		"exception traceback should land in stderr")

	stdout, _, _ := eval(t, s, "print(marker)")
	require.Equal(t, "alive\n", stdout,
		"prior state must survive an exception in the user code")
}

func TestReset_BumpsEpochAndClearsState(t *testing.T) {
	s := newServer(t)

	eval(t, s, "x = 'before reset'")
	_, _, epochBefore := eval(t, s, "x")

	resetArgs, err := structpb.NewStruct(map[string]any{})
	require.NoError(t, err)
	resetResp, err := s.CallTool(context.Background(), &toolboxv0.CallToolRequest{
		Name:      "python.repl.reset",
		Arguments: resetArgs,
	})
	require.NoError(t, err)
	require.Empty(t, resetResp.Error)

	// After reset, x should be undefined → eval surfaces NameError on stderr.
	_, stderr, epochAfter := eval(t, s, "print(x)")
	require.Contains(t, stderr, "NameError",
		"reset should have wiped globals; x is undefined now")
	require.Greater(t, epochAfter, epochBefore,
		"session_epoch must increase after reset so callers can detect state loss")
}

func TestEval_RejectsEmptyCode(t *testing.T) {
	s := newServer(t)

	args, err := structpb.NewStruct(map[string]any{"code": ""})
	require.NoError(t, err)
	resp, err := s.CallTool(context.Background(), &toolboxv0.CallToolRequest{
		Name:      "python.repl.eval",
		Arguments: args,
	})
	require.NoError(t, err)
	require.Contains(t, resp.Error, "code is required")
}

// Package pythonrepl is a codefly toolbox plugin that hosts a
// persistent `python -i` subprocess and exposes it as typed RPCs.
//
// REPL state (variables, imports, defined functions) lives in the
// subprocess; the subprocess lives for the plugin process's
// lifetime; the plugin process is kept alive by the codefly host
// (loaded once, talked to over UDS). That stack is what makes a
// REPL implementable as a tool — a stateless tool function can't
// host one.
//
// If the plugin restarts (crash, host shutdown), state is lost.
// session_epoch in every eval response increments at startup so
// the LLM can detect the loss and re-establish.
package pythonrepl

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	toolboxv0 "github.com/codefly-dev/core/generated/go/codefly/services/toolbox/v0"
	"github.com/codefly-dev/core/toolbox/registry"
)

// sentinelStdout is printed by the helper after every eval; the
// reader scans for it to know "evaluation complete." Chosen to be
// extremely unlikely to appear in user output.
const sentinelStdout = "<<__codefly_repl_done__>>"

// helperScript is the Python program the subprocess runs in a loop.
// It reads len-prefixed code blocks from stdin, execs them in a
// persistent globals dict, and prints a sentinel after each block
// so the Go reader knows the eval is done. Using a length prefix
// avoids the "what if user code prints the sentinel" problem.
const helperScript = `
import sys, io, traceback, json
GLOBALS = {"__name__": "__codefly_repl__"}
SENTINEL_OUT = "<<__codefly_repl_done__>>"

def read_block():
    line = sys.stdin.readline()
    if not line:
        return None
    n = int(line.strip())
    return sys.stdin.read(n)

while True:
    code = read_block()
    if code is None:
        break
    out_buf = io.StringIO()
    err_buf = io.StringIO()
    real_out, real_err = sys.stdout, sys.stderr
    sys.stdout, sys.stderr = out_buf, err_buf
    try:
        # Try eval first (so simple expressions return a value);
        # fall back to exec for statements.
        try:
            value = eval(code, GLOBALS)
            if value is not None:
                print(repr(value))
        except SyntaxError:
            exec(code, GLOBALS)
        result_err = ""
    except BaseException:
        result_err = traceback.format_exc()
    finally:
        sys.stdout, sys.stderr = real_out, real_err
    payload = json.dumps({
        "stdout": out_buf.getvalue(),
        "stderr": err_buf.getvalue() + result_err,
    })
    sys.stdout.write(payload)
    sys.stdout.write("\n")
    sys.stdout.write(SENTINEL_OUT)
    sys.stdout.write("\n")
    sys.stdout.flush()
`

// session is the plugin's interface to the live python subprocess.
// One session per plugin process for now; multi-session-per-process
// is a future enhancement (keyed by session_id in the request).
type session struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	epoch   int64 // increments on every (re)start; surfaces in eval responses
	pythonBin string
}

// Server is the toolbox plugin. Embeds *registry.Base for the four
// boilerplate RPCs; owns Identity + Tools + per-tool handlers.
type Server struct {
	*registry.Base

	version string
	sess    *session
}

// New constructs a Server. The python binary is resolved from PATH
// unless CODEFLY_TOOLBOX_PYTHON_BIN is set. The subprocess is NOT
// started here — it's started lazily on first eval, so the plugin
// can boot even when python isn't installed (Identity still works).
func New(version, pythonBin string) *Server {
	s := &Server{
		version: version,
		sess:    &session{pythonBin: pythonBin},
	}
	s.Base = registry.NewBase(s)
	return s
}

func (s *Server) Identity(_ context.Context, _ *toolboxv0.IdentityRequest) (*toolboxv0.IdentityResponse, error) {
	return &toolboxv0.IdentityResponse{
		Name:           "python-repl",
		Version:        s.version,
		Description:    "Persistent Python REPL — variables and imports survive across eval calls.",
		CanonicalFor:   []string{},
		SandboxSummary: "needs python on PATH; reads workspace; network deny",
	}, nil
}

// Close terminates the python subprocess if it's running. Idempotent.
// Called by the host on plugin shutdown via the agents.Serve lifecycle.
func (s *Server) Close() error {
	return s.sess.kill()
}

// --- Tools ----------------------------------------------------

func (s *Server) Tools() []*registry.ToolDefinition {
	return []*registry.ToolDefinition{
		{
			Name:               "python.repl.eval",
			SummaryDescription: "Execute Python code in a persistent REPL session. Variables and imports persist across calls.",
			LongDescription: "Runs `code` in a long-lived `python -i` subprocess managed by the plugin. " +
				"Globals (variables, imports, defined functions/classes) persist across calls within a " +
				"single plugin lifetime. Returns stdout, stderr, and the session_epoch — if the epoch " +
				"changes between calls, the subprocess restarted and prior state is lost.\n\n" +
				"Use for incremental experimentation, data exploration, and short scripts. For long " +
				"scripts, write to a file and execute that file in the REPL.\n\n" +
				"There is a per-call timeout (default 30s) — runaway code will be interrupted and the " +
				"session may be reset.",
			InputSchema: mustSchema(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"code": map[string]any{
						"type":        "string",
						"description": "Python source. Either an expression (return value is repr'd to stdout) or a statement / block.",
					},
					"timeout_seconds": map[string]any{
						"type":        "number",
						"description": "Per-call timeout. Default 30. Hard cap 300.",
					},
				},
				"required": []any{"code"},
			}),
			Tags:        []string{"python", "repl", "destructive"},
			Destructive: true,
			Idempotency: "side_effecting",
			ErrorModes:  "Returns `start: ...` when the python subprocess can't be launched (binary missing, sandbox blocks); `timeout` when a single eval exceeds timeout_seconds; stdout/stderr embedded in the response on Python exceptions.",
			Examples: []*toolboxv0.ToolExample{
				{
					Description:     "Define a variable, then read it in a later call.",
					Arguments:       mustStruct(map[string]any{"code": "x = 42"}),
					ExpectedOutcome: "{ stdout: '', stderr: '', session_epoch: 1 }. A subsequent call with code='print(x)' prints 42.",
				},
				{
					Description:     "Evaluate an expression — the repr is printed.",
					Arguments:       mustStruct(map[string]any{"code": "1 + 2"}),
					ExpectedOutcome: "{ stdout: '3\\n', stderr: '', session_epoch: 1 }",
				},
				{
					Description:     "Catch a syntax/runtime error.",
					Arguments:       mustStruct(map[string]any{"code": "1/0"}),
					ExpectedOutcome: "stderr contains 'ZeroDivisionError: division by zero', session continues normally.",
				},
			},
			Handler: s.eval,
		},
		{
			Name:               "python.repl.reset",
			SummaryDescription: "Discard the current REPL session and start a fresh one. Increments session_epoch.",
			LongDescription: "Forcibly terminates the underlying python subprocess and starts a new one on " +
				"the next eval call. Use to clear all globals, imports, and recover from a stuck session. " +
				"After reset, the next eval response carries a higher session_epoch — agents detecting " +
				"this can re-establish whatever state they need.",
			InputSchema: mustSchema(map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}),
			Tags:        []string{"python", "repl", "destructive"},
			Destructive: true,
			Idempotency: "side_effecting",
			ErrorModes:  "Always succeeds (kill is best-effort; a fresh process is started lazily on next eval).",
			Examples: []*toolboxv0.ToolExample{
				{
					Description:     "Wipe state after defining a bunch of throwaway vars.",
					Arguments:       mustStruct(map[string]any{}),
					ExpectedOutcome: "{ ok: true, new_epoch: <prev+1> }",
				},
			},
			Handler: s.reset,
		},
	}
}

// --- Handlers -------------------------------------------------

func (s *Server) eval(_ context.Context, req *toolboxv0.CallToolRequest) *toolboxv0.CallToolResponse {
	args := argsMap(req)
	code, _ := args["code"].(string)
	if code == "" {
		return errResp("python.repl.eval: code is required")
	}
	timeout := 30 * time.Second
	if v, ok := args["timeout_seconds"].(float64); ok && v > 0 {
		if v > 300 {
			v = 300
		}
		timeout = time.Duration(v * float64(time.Second))
	}

	if err := s.sess.ensureStarted(); err != nil {
		return errResp("start: %v", err)
	}

	stdout, stderr, err := s.sess.eval(code, timeout)
	if err != nil {
		// Reset on protocol-level failure (broken pipe, unparseable
		// output) so the next call gets a clean process.
		_ = s.sess.kill()
		return errResp("eval: %v", err)
	}
	return structResp(map[string]any{
		"stdout":        stdout,
		"stderr":        stderr,
		"session_epoch": s.sess.currentEpoch(),
	})
}

func (s *Server) reset(_ context.Context, _ *toolboxv0.CallToolRequest) *toolboxv0.CallToolResponse {
	prev := s.sess.currentEpoch()
	_ = s.sess.kill()
	return structResp(map[string]any{
		"ok":        true,
		"new_epoch": prev + 1, // ensureStarted on next eval bumps to this
	})
}

// --- session implementation ----------------------------------

func (s *session) ensureStarted() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd != nil && s.cmd.Process != nil {
		// Process is alive (we don't poll, but if it died the
		// next read from stdout will fail and we restart).
		return nil
	}
	bin := s.pythonBin
	if bin == "" {
		bin = "python3"
	}
	cmd := exec.Command(bin, "-u", "-c", helperScript)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", bin, err)
	}
	s.cmd = cmd
	s.stdin = stdin
	s.stdout = bufio.NewReader(stdout)
	s.epoch++
	return nil
}

func (s *session) eval(code string, timeout time.Duration) (stdout, stderr string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cmd == nil {
		return "", "", errors.New("session not started")
	}

	// Write length-prefixed code block.
	if _, err := fmt.Fprintf(s.stdin, "%d\n%s", len(code), code); err != nil {
		return "", "", fmt.Errorf("write: %w", err)
	}

	// Read until sentinel, with a soft timeout. We don't kill on
	// timeout — a bigger framework would; for now we just surface
	// the timeout and let the caller decide whether to reset.
	type result struct {
		stdout, stderr string
		err            error
	}
	done := make(chan result, 1)
	go func() {
		var lines []string
		for {
			line, err := s.stdout.ReadString('\n')
			if err != nil {
				done <- result{err: fmt.Errorf("read: %w", err)}
				return
			}
			if strings.HasPrefix(line, sentinelStdout) {
				break
			}
			lines = append(lines, line)
		}
		// The helper writes a single JSON line per eval. Take the
		// last non-sentinel line — earlier lines would only appear
		// if the helper itself printed extra (it doesn't today).
		if len(lines) == 0 {
			done <- result{err: errors.New("no payload from helper")}
			return
		}
		payload := lines[len(lines)-1]
		// Inline tiny JSON decode — avoid pulling encoding/json
		// just for {stdout, stderr}; the helper format is fixed.
		out, errOut, perr := parseHelperPayload(payload)
		done <- result{stdout: out, stderr: errOut, err: perr}
	}()

	select {
	case r := <-done:
		return r.stdout, r.stderr, r.err
	case <-time.After(timeout):
		return "", "", fmt.Errorf("timeout after %s", timeout)
	}
}

func (s *session) kill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	_ = s.stdin.Close()
	_ = s.cmd.Process.Kill()
	_ = s.cmd.Wait()
	s.cmd = nil
	s.stdin = nil
	s.stdout = nil
	return nil
}

func (s *session) currentEpoch() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch
}

// parseHelperPayload extracts stdout/stderr from the helper's
// single-line JSON. We hand-roll it to keep dependencies minimal —
// the helper format is fixed and {stdout, stderr} are both strings.
func parseHelperPayload(line string) (stdout, stderr string, err error) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return "", "", fmt.Errorf("malformed payload: %q", line)
	}
	// Brittle but sufficient: encoding/json would be cleaner. Worth
	// switching if we ever surface fields beyond the two strings.
	stdout = extractJSONString(line, `"stdout":`)
	stderr = extractJSONString(line, `"stderr":`)
	return stdout, stderr, nil
}

func extractJSONString(s, key string) string {
	idx := strings.Index(s, key)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(key):]
	// Skip whitespace + opening quote.
	for len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t') {
		rest = rest[1:]
	}
	if len(rest) == 0 || rest[0] != '"' {
		return ""
	}
	rest = rest[1:]
	var out strings.Builder
	for i := 0; i < len(rest); i++ {
		c := rest[i]
		if c == '\\' && i+1 < len(rest) {
			switch rest[i+1] {
			case 'n':
				out.WriteByte('\n')
			case 't':
				out.WriteByte('\t')
			case 'r':
				out.WriteByte('\r')
			case '"':
				out.WriteByte('"')
			case '\\':
				out.WriteByte('\\')
			default:
				out.WriteByte(rest[i+1])
			}
			i++
			continue
		}
		if c == '"' {
			return out.String()
		}
		out.WriteByte(c)
	}
	return out.String()
}

// --- structpb helpers ----------------------------------------

func argsMap(req *toolboxv0.CallToolRequest) map[string]any {
	if req == nil || req.Arguments == nil {
		return map[string]any{}
	}
	return req.Arguments.AsMap()
}

func errResp(format string, a ...any) *toolboxv0.CallToolResponse {
	return &toolboxv0.CallToolResponse{Error: fmt.Sprintf(format, a...)}
}

func structResp(payload map[string]any) *toolboxv0.CallToolResponse {
	pb, err := structpb.NewStruct(payload)
	if err != nil {
		return errResp("encode response: %v", err)
	}
	return &toolboxv0.CallToolResponse{
		Content: []*toolboxv0.Content{
			{Body: &toolboxv0.Content_Structured{Structured: pb}},
		},
	}
}

func mustStruct(m map[string]any) *structpb.Struct {
	s, err := structpb.NewStruct(m)
	if err != nil {
		panic(fmt.Sprintf("python-repl toolbox: cannot encode example args: %v", err))
	}
	return s
}

func mustSchema(m map[string]any) *structpb.Struct { return mustStruct(m) }

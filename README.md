# toolbox-python-repl

A codefly toolbox plugin that hosts a persistent Python REPL session.
Variables, imports, and defined functions persist across `python.repl.eval`
calls — the plugin keeps a long-lived `python -i` subprocess in memory.

## Why this is a plugin (not a tool function)

A stateless tool function can't host a REPL. Codefly's plugin model keeps
the binary alive on a Unix domain socket, which keeps the Python interpreter
alive — statefulness lives in the plugin process, the wire shape stays
clean RPC.

## Tools

- `python.repl.eval(code, timeout_seconds?)` — run code, return stdout/stderr
  and a `session_epoch` int. Variables, imports, and defined functions
  defined in one call are visible in the next.
- `python.repl.reset()` — terminate the underlying subprocess. Next eval
  starts fresh; `session_epoch` increments so callers can detect state loss.

## State semantics

- The Python subprocess is started lazily on first eval and lives for the
  plugin process's lifetime.
- If the plugin crashes (or codefly restarts the binary), REPL state is lost.
  The `session_epoch` field in eval responses surfaces this — when it
  increments between calls, prior state is gone.

## Configuration

| Env var                       | Default       | Purpose                                            |
| ----------------------------- | ------------- | -------------------------------------------------- |
| `CODEFLY_TOOLBOX_VERSION`     | `0.0.0-dev`   | Identity version surfaced via `Identity()`         |
| `CODEFLY_TOOLBOX_PYTHON_BIN`  | `python3`     | Override the python binary path (mostly for tests) |

## Build & test

```bash
go build ./...
go test ./...                          # metadata-only tests
go test -tags=python_required ./...    # full REPL behavior tests; needs python3
```

## Contract

This plugin implements the codefly Toolbox gRPC contract defined in
[`codefly-dev/core`](https://github.com/codefly-dev/core) at
`proto/codefly/services/toolbox/v0/toolbox.proto`. Loaded by the codefly
host via `agents.Serve` over a Unix domain socket.

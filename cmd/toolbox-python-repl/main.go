// Command toolbox-python-repl is the standalone binary form of the
// codefly python-repl toolbox plugin. Loaded via the standard agent
// loader (core/agents/manager.Load); registers a Toolbox server
// through agents.Serve.
//
// Configuration:
//
//	CODEFLY_TOOLBOX_VERSION    — Identity version. Default "0.0.0-dev".
//	CODEFLY_TOOLBOX_PYTHON_BIN — override the python binary path.
//	                              Default: empty (resolves "python3"
//	                              from PATH).
//
// State semantics: the python subprocess is started lazily on first
// eval and lives for the plugin process's lifetime. If the plugin
// crashes or codefly restarts the binary, REPL state is lost; the
// session_epoch in eval responses surfaces this to callers.
package main

import (
	"github.com/codefly-dev/core/agents"
	coretoolbox "github.com/codefly-dev/core/toolbox"
	pythonrepl "github.com/codefly-dev/toolbox-python-repl"
)

func main() {
	server := pythonrepl.New(coretoolbox.Version(), coretoolbox.Environment("CODEFLY_TOOLBOX_PYTHON_BIN", ""))
	agents.ServeToolbox(server)
}

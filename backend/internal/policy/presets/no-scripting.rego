# Everything in the browser except running script in the page or replacing
# its content. No desktop control and no shell.
package browserjs.policy

import rego.v1

# Desktop control and the shell are denied, and must stay denied in a policy
# like this one: desktop_execute can type into the address bar or DevTools,
# and a shell command can reach the browser's own control ports, so either
# would drive the browser around the rules below.
allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
	is_array(input.arguments.operations)
	every op in input.arguments.operations {
		operation_allowed(op)
	}
}

# Every operation of browser_execute but evaluate and setContent.
operation_allowed(op) if op.type in {"click", "navigate", "press", "screenshot", "select", "setViewport", "type", "url", "wait"}

# Look, do not touch: open https pages, wait, and take screenshots. No
# desktop control and no shell.
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

operation_allowed(op) if op.type in {"screenshot", "setViewport", "url", "wait"}

# navigate, to a plain https URL: no userinfo, no backslash, no whitespace.
operation_allowed(op) if {
	op.type == "navigate"
	is_string(op.params.url)
	regex.match(`^https://[a-z0-9.-]+(?::[0-9]+)?(?:[/?#].*)?$`, lower(op.params.url))
}

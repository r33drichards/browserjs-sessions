# Generated from a browserjs JSON policy (version 1). Edit the JSON, not this file.
package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
	is_array(input.arguments.operations)
	every op in input.arguments.operations {
		operation_allowed(op)
	}
}

allowed_operations := {"click", "evaluate", "navigate", "press", "screenshot", "select", "setContent", "setViewport", "type", "url", "wait"}

operation_allowed(op) if {
	op.type in allowed_operations
}

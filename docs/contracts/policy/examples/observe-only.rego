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

allowed_operations := {"screenshot", "setViewport", "url", "wait"}

operation_allowed(op) if {
	op.type in allowed_operations
}

# allow.rules[0]
operation_allowed(op) if {
	op.type == "navigate"
	is_string(op.params["url"])
	regex.match("^(?:https)://[a-z0-9.-]+(?::[0-9]+)?(?:[/?#].*)?$", lower(op.params["url"]))
}

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

denied_operations := {"evaluate", "setContent"}

allowed_operations := {"click", "screenshot", "select", "url", "wait"}

operation_allowed(op) if {
	not op.type in denied_operations
	op.type in allowed_operations
}

# allow.rules[0]
operation_allowed(op) if {
	not op.type in denied_operations
	op.type == "navigate"
	is_string(op.params["url"])
	regex.match("^(?:http|https)://(?:(?:[a-z0-9-]+\\.)+intranet\\.example\\.org|forms\\.example\\.org)(?::[0-9]+)?(?:[/?#].*)?$", lower(op.params["url"]))
}

# allow.rules[1]
operation_allowed(op) if {
	not op.type in denied_operations
	op.type == "type"
	is_string(op.params["text"])
	count(op.params["text"]) <= 200
	regex.match("^[\\x20-\\x7E]*$", op.params["text"])
}

# allow.rules[2]
operation_allowed(op) if {
	not op.type in denied_operations
	op.type == "press"
	op.params["key"] in {"Enter", "Escape", "Tab"}
}

# allow.rules[3]
operation_allowed(op) if {
	not op.type in denied_operations
	op.type == "setViewport"
	is_number(op.params["height"])
	op.params["height"] >= 200
	op.params["height"] <= 1200
	is_number(op.params["width"])
	op.params["width"] >= 320
	op.params["width"] <= 1920
}

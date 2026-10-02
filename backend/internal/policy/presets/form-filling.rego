# Fill in forms on two sites: printable text only, a few keys, sane viewport
# sizes, no script. No desktop control and no shell.
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

operation_allowed(op) if op.type in {"click", "screenshot", "select", "url", "wait"}

# navigate, to forms.example.org or a subdomain of intranet.example.org.
operation_allowed(op) if {
	op.type == "navigate"
	is_string(op.params.url)
	regex.match(`^https?://(?:(?:[a-z0-9-]+\.)+intranet\.example\.org|forms\.example\.org)(?::[0-9]+)?(?:[/?#].*)?$`, lower(op.params.url))
}

# type, up to 200 printable ASCII characters.
operation_allowed(op) if {
	op.type == "type"
	is_string(op.params.text)
	count(op.params.text) <= 200
	regex.match(`^[\x20-\x7E]*$`, op.params.text)
}

operation_allowed(op) if {
	op.type == "press"
	op.params.key in {"Enter", "Escape", "Tab"}
}

operation_allowed(op) if {
	op.type == "setViewport"
	is_number(op.params.width)
	op.params.width >= 320
	op.params.width <= 1920
	is_number(op.params.height)
	op.params.height >= 200
	op.params.height <= 1200
}

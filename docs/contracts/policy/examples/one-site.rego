# The agent may be sent only to example.com and its subdomains, and may type
# only short text. No desktop control and no shell.
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

operation_allowed(op) if op.type in {"click", "press", "screenshot", "select", "setViewport", "url", "wait"}

# navigate, over https, to example.com or a subdomain of it. The expression
# is matched against the whole lower-cased URL and admits only plain ones:
# userinfo (https://example.com@evil.test/), a backslash, whitespace, a
# percent-encoded or non-ASCII host and a trailing dot all fail.
operation_allowed(op) if {
	op.type == "navigate"
	is_string(op.params.url)
	regex.match(`^https://(?:(?:[a-z0-9-]+\.)+example\.com|example\.com)(?::[0-9]+)?(?:[/?#].*)?$`, lower(op.params.url))
}

# type, at most 500 characters.
operation_allowed(op) if {
	op.type == "type"
	is_string(op.params.text)
	count(op.params.text) <= 500
}

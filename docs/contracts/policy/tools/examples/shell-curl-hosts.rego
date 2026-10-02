# `curl`, to two hosts over https, and nothing else. Every argument is
# accounted for: `-q` first (no ~/.curlrc), then only flags from a short list
# of ones that take no value, and URLs whose host is on the list. Anything
# this policy does not recognise (-o, -L, -x, -K, --resolve, --connect-to,
# a proxy variable in `env`, ...) denies the call.
package browserjs.policy

import rego.v1

allowed_hosts := {"api.github.com", "example.com"}

# Flags without a value. Not -L: a redirect leads to whatever host the
# server names.
plain_flags := {"-s", "-S", "-f", "-i", "-I", "--silent", "--show-error", "--fail", "--include", "--head", "--compressed"}

understood_fields := {"argv", "timeout_ms", "max_output_bytes"}

allow_tool_call if {
	input.server == "browser"
	input.tool == "shell_execute"
	is_object(input.arguments)
	count(object.keys(input.arguments) - understood_fields) == 0
	argv := input.arguments.argv
	is_array(argv)
	count(argv) >= 3
	argv[0] == "curl"
	argv[1] == "-q"
	rest := array.slice(argv, 2, count(argv))
	every arg in rest {
		argument_allowed(arg)
	}
	some arg in rest
	url_allowed(arg)
}

argument_allowed(arg) if arg in plain_flags

argument_allowed(arg) if url_allowed(arg)

# https, the host exactly, then the end or a path. No userinfo
# ("https://example.com@evil.test/"), no port, no upper case, and no
# characters curl expands into several URLs ("{a,b}", "[1-9]").
url_allowed(arg) if {
	is_string(arg)
	parts := regex.find_all_string_submatch_n(`^https://([a-z0-9.-]+)(/[^\s{}\[\]\\]*)?$`, arg, 1)
	count(parts) == 1
	parts[0][1] in allowed_hosts
}

allow_tool_call if {
	input.server == "browser"
	input.tool == "shell_process"
}

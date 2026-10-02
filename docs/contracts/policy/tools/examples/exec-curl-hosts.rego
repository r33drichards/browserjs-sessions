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

allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	only_fields({"bin", "args", "timeout"})
	input.arguments.bin == "curl"
	count(call_args) >= 2
	call_args[0] == "-q"
	rest := array.slice(call_args, 1, count(call_args))
	every arg in rest {
		argument_allowed(arg)
	}
	some arg in rest
	url_allowed(arg)
	timeout_within(120)
}

argument_allowed(arg) if arg in plain_flags

argument_allowed(arg) if url_allowed(arg)

# https, the host exactly, then the end or a path. No userinfo
# ("https://example.com@evil.test/"), no port, no upper case, and no
# characters curl expands into several URLs ("{a,b}", "[1-9]").
url_allowed(arg) if {
	parts := regex.find_all_string_submatch_n(`^https://([a-z0-9.-]+)(/[^\s{}\[\]\\]*)?$`, arg, 1)
	count(parts) == 1
	parts[0][1] in allowed_hosts
}

# The arguments, whether the call gave `args` or left it out (the server
# treats both as none). Undefined when `args` is not an array of strings, which
# denies.
call_args := args if {
	args := object.get(input.arguments, "args", [])
	is_array(args)
	every a in args {
		is_string(a)
	}
}

# Seconds. The server has no maximum of its own.
timeout_within(limit) if {
	is_number(input.arguments.timeout)
	input.arguments.timeout >= 1
	input.arguments.timeout <= limit
}

# The call has no field this policy has not looked at. `env` among them: it
# can set PATH, which decides what a program name means.
only_fields(known) if {
	is_object(input.arguments)
	count(object.keys(input.arguments) - known) == 0
}

# Reading the output of a command, and stopping one, start nothing.
allow_tool_call if {
	input.server == "exec"
	input.tool in {"stream_logs", "search_logs", "kill"}
}


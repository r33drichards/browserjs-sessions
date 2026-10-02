# `curl`, to two hosts over https, and nothing else. The whole command line is
# one expression: `curl -q` (no ~/.curlrc), flags from a short list of ones
# that take no value, then one URL in single quotes whose host is on the
# list. Anything else (-o, -L, -x, -K, --resolve, a second command) does not
# match and is denied.
package browserjs.policy

import rego.v1

# The URL is single-quoted, where the shell interprets nothing, so it may
# contain ? and & ; it may contain anything printable but a single quote.
# The host is followed by the end or a "/": no userinfo, no port.
# Not -L: a redirect leads to whatever host the server names.
curl_command := `^curl -q( (-s|-S|-f|-i|-I|--silent|--show-error|--fail|--include|--head|--compressed))* 'https://(api\.github\.com|example\.com)(/[!-&(-~]*)?'$`

allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	is_object(input.arguments)
	is_string(input.arguments.cmd)
	regex.match(curl_command, input.arguments.cmd)
	is_number(input.arguments.timeout)
	input.arguments.timeout >= 1
	input.arguments.timeout <= 120
}

# Reading the output of a command starts nothing.
allow_tool_call if {
	input.server == "exec"
	input.tool in {"stream_logs", "search_logs"}
}


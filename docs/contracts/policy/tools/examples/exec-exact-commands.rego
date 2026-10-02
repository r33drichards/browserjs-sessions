# Only these command lines, character for character, for at most ten minutes
# each. Nothing else in the session: no browser or desktop tool.
#
# The one policy on `exec` that needs no knowledge of the shell: the string
# is on the list or it is not.
package browserjs.policy

import rego.v1

allowed_commands := {
	"git -C /home/browser/work/app pull --ff-only",
	"cd /home/browser/work/app && npm ci && npm test",
	"df -h /data/chrome",
}

allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	is_object(input.arguments)
	input.arguments.cmd in allowed_commands
	timeout_within(600)
}

# Seconds. The server has no maximum of its own.
timeout_within(limit) if {
	is_number(input.arguments.timeout)
	input.arguments.timeout >= 1
	input.arguments.timeout <= limit
}

# Reading the output of a command starts nothing.
allow_tool_call if {
	input.server == "exec"
	input.tool in {"stream_logs", "search_logs"}
}


# Only these commands, program and arguments exactly, for at most ten minutes
# each, in these directories. Nothing else in the session: no browser or
# desktop tool.
#
# The strongest policy on `exec`, and the one to start from: a call is on
# the list or it is not.
package browserjs.policy

import rego.v1

allowed_commands := {
	{"bin": "git", "args": ["pull", "--ff-only"], "cwd": "/data/chrome/home/work/app"},
	{"bin": "npm", "args": ["test"], "cwd": "/data/chrome/home/work/app"},
	{"bin": "df", "args": ["-h", "/data/chrome"]},
}

allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	only_fields({"bin", "args", "timeout", "cwd"})
	command in allowed_commands
	timeout_within(600)
}

# The call as an entry of the list: `cwd` only when it was given.
command := object.union({"bin": input.arguments.bin, "args": call_args}, cwd_given)

cwd_given := {"cwd": input.arguments.cwd} if "cwd" in object.keys(input.arguments)

cwd_given := {} if not "cwd" in object.keys(input.arguments)

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


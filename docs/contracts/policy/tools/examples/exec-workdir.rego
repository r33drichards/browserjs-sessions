# Commands only with a working directory inside /data/chrome/home/work, named in
# the call, and only these extra environment variables.
#
# Any program: this policy is about where, not what. And the directory
# decides where a command starts, not what it can touch: its arguments can
# name any path the user can read ("cat /data/chrome/..."). Also,
# mcp-exec uses `cwd` as written: it does not resolve symbolic links, so a
# link inside the directory that points out of it is followed. The policy
# refuses the spellings that leave by themselves (".." and ".").
package browserjs.policy

import rego.v1

workdir := "/data/chrome/home/work"

# Never PATH, LD_PRELOAD or the like: they change what a program name means
# and what gets loaded into it.
allowed_env := {"CI", "NODE_ENV", "GIT_TERMINAL_PROMPT", "TZ", "LANG"}

allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	only_fields({"bin", "args", "timeout", "cwd", "env"})
	is_string(input.arguments.bin)

	call_args

	# Without cwd the command starts in the home directory: denied.
	cwd := input.arguments.cwd
	is_string(cwd)
	inside_workdir(cwd)
	env_allowed
	timeout_within(1800)
}

inside_workdir(cwd) if cwd == workdir

inside_workdir(cwd) if {
	startswith(cwd, concat("", [workdir, "/"]))
	segments := split(trim_prefix(cwd, concat("", [workdir, "/"])), "/")
	every s in segments {
		not s in {"", ".", ".."}
	}
}

env_allowed if not "env" in object.keys(input.arguments)

env_allowed if {
	is_object(input.arguments.env)
	count(object.keys(input.arguments.env) - allowed_env) == 0
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


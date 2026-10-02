# Everything in the browser, and a short list of read-only programs: pwd,
# ls, cat of files below the home directory, and git status, log, diff and
# show. No desktop control.
package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
}

# Desktop control is denied, and must stay denied in a policy that restricts
# the shell: desktop_execute can open a terminal and type any command.

# exec runs a program directly, with no shell: `bin` is the program and
# `args` its arguments, exactly as given. This is a list of entry points,
# not a sandbox: what an allowed program does is up to the program (git runs
# what the repository's config names), so no shell, interpreter or launcher
# (sh, bash, env, xargs, python3) is on the list.
allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"

	# No field this policy has not looked at. That refuses `env`, which
	# could set PATH and change what a program name means, and `cwd`: the
	# program runs in the home directory.
	is_object(input.arguments)
	count(object.keys(input.arguments) - {"bin", "args", "timeout"}) == 0
	command_allowed(input.arguments.bin, call_args)

	# Seconds. The server has no maximum of its own.
	is_number(input.arguments.timeout)
	input.arguments.timeout >= 1
	input.arguments.timeout <= 60
}

# The arguments, whether the call gave `args` or left it out (the server
# treats both as none). Undefined, which denies, when it is not an array of
# strings.
call_args := args if {
	args := object.get(input.arguments, "args", [])
	is_array(args)
	every arg in args {
		is_string(arg)
	}
}

# The program is compared as a whole string: "git" is found on the desktop's
# PATH; "/tmp/git" and "./git" are other strings, and denied.
command_allowed("pwd", args) if count(args) == 0

command_allowed("ls", args) if {
	every arg in args {
		ls_argument(arg)
	}
}

ls_argument(arg) if arg in {"-l", "-a", "-la", "-al"}

ls_argument(arg) if relative_path(arg)

command_allowed("cat", args) if {
	count(args) >= 1
	every arg in args {
		relative_path(arg)
	}
}

# The subcommand at args[0] keeps git's own options (-c, -C, --exec-path,
# which come before it) out. After it, a flag must be one of those listed;
# anything else must not begin with "-".
command_allowed("git", args) if {
	args[0] in {"status", "log", "diff", "show"}
	every arg in array.slice(args, 1, count(args)) {
		regex.match(`^(--oneline|--stat|--name-only|--cached|-n|[^-].*)$`, arg)
	}
}

# A path below the working directory: no leading "/", no "..", no hidden
# name, and only letters, digits, "_", "." and "-" in each part.
relative_path(arg) if regex.match(`^[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`, arg)

# Reading the output of a command that was started, and stopping one. These
# start nothing.
allow_tool_call if {
	input.server == "exec"
	input.tool in {"stream_logs", "search_logs", "kill"}
}

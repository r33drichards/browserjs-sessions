# `git`, and only subcommands that read: status, log, diff, show, rev-parse,
# ls-files. Nothing else in the session.
#
# Three patterns: the program compared as a whole string; the subcommand at
# args[0], which also keeps git's own options (-c, -C, --exec-path, which
# come before the subcommand) out; and flags denied anywhere after it.
#
# The denied flags are the weak part: a list of what its author knew about.
# (Git accepts any unambiguous abbreviation of a long flag, "--outp" for
# "--output", which is why the rule below matches prefixes.) And git reads
# the configuration of the repository it is run in, which can name programs
# for git to run (core.fsmonitor, diff drivers): this policy is safe for
# repositories the agent cannot write to, not for arbitrary ones.
package browserjs.policy

import rego.v1

allowed_subcommands := {"status", "log", "diff", "show", "rev-parse", "ls-files"}

# Long flags of those subcommands that write a file, run a configured
# program, or read files outside the repository.
denied_flags := {"--output", "--ext-diff", "--textconv", "--no-index"}

allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	only_fields({"bin", "args", "timeout", "cwd"})

	# By name: found on the desktop's PATH. "/tmp/git" and "./git" are other
	# strings, and denied.
	input.arguments.bin == "git"
	call_args[0] in allowed_subcommands
	every arg in call_args {
		not flag_denied(arg)
	}
	timeout_within(120)
}

# "--output", "--output=x", and every abbreviation git would take for it.
flag_denied(arg) if {
	startswith(arg, "--")
	name := split(arg, "=")[0]
	count(name) > 2
	some flag in denied_flags
	startswith(flag, name)
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


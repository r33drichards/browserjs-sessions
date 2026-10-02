# Everything in the browser, and a short list of read-only shell commands:
# pwd, ls, a few git commands, and cat of a file below the working
# directory. No desktop control.
package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
}

# Desktop control is denied, and must stay denied in a policy that restricts
# the shell: desktop_execute can open a terminal and type any command.

# exec runs `sh -c <cmd>`. A policy sees the command as one string: there is
# no program and argument list to check, so the only sound rules are whole
# commands, or an expression anchored at both ends that admits no shell
# metacharacter (; | & $ ` > < ( ) newline, quotes, spaces where none are
# meant). This is a list of commands, not a sandbox: what a listed command
# does is up to the program (git runs what the repository's config names).
allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	is_string(input.arguments.cmd)
	command_allowed(input.arguments.cmd)
	is_number(input.arguments.timeout)
	input.arguments.timeout >= 1
	input.arguments.timeout <= 60
}

command_allowed(cmd) if cmd in {"pwd", "ls", "ls -la", "git status", "git log --oneline -n 20", "git diff --stat"}

# cat of one relative path with no "..", no hidden name and nothing but
# letters, digits, "_", "." and "-" in each part.
command_allowed(cmd) if regex.match(`^cat [A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*$`, cmd)

# Reading the output of a command that was started. These start nothing.
allow_tool_call if {
	input.server == "exec"
	input.tool in {"stream_logs", "search_logs"}
}

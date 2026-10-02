# Any one program with plain-word arguments; no shell syntax at all (no pipes,
# redirection, substitution, quoting, several commands). The other tools are
# allowed.
#
# This is a rule about form, not about what can run: `sh /tmp/x.sh`,
# `env …`, `xargs …` and `python3 file.py` are single programs with plain
# arguments. Its use is that every command is then a program and arguments
# that a stricter policy, or a person reading the audit trail, can read; add
# a list of programs (exec-git-ls.rego) to restrict what runs.
package browserjs.policy

import rego.v1

# The program: no "=" in it, or "A=b program" would set a variable.
plain_command := `^[A-Za-z0-9_./+-]+( [A-Za-z0-9_./:=@%+,-]+)*$`

allow_tool_call if {
	input.server == "browser"
	input.tool in {"browser_execute", "desktop_execute"}
}

allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	is_object(input.arguments)
	is_string(input.arguments.cmd)
	regex.match(plain_command, input.arguments.cmd)
}

# Reading the output of a command starts nothing.
allow_tool_call if {
	input.server == "exec"
	input.tool in {"stream_logs", "search_logs"}
}


# Commands only in /home/browser/work or below. `exec` has no working
# directory argument, so the policy requires the command line to begin with
# the `cd` itself: "cd <directory> && <one program with plain arguments>".
#
# This decides where a command starts, not what it can touch: its arguments
# can name any absolute path the user can read. ".." is refused everywhere in
# the string, so neither the directory nor a relative argument climbs out.
package browserjs.policy

import rego.v1

in_workdir := `^cd /home/browser/work(/[A-Za-z0-9_.-]+)* && [A-Za-z0-9_./+-]+( [A-Za-z0-9_./:=@%+,-]+)*$`

allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	is_object(input.arguments)
	is_string(input.arguments.cmd)
	regex.match(in_workdir, input.arguments.cmd)
	not contains(input.arguments.cmd, "..")
	is_number(input.arguments.timeout)
	input.arguments.timeout >= 1
	input.arguments.timeout <= 1800
}

# Reading the output of a command starts nothing.
allow_tool_call if {
	input.server == "exec"
	input.tool in {"stream_logs", "search_logs"}
}


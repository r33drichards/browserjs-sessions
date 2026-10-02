# Commands only with a working directory inside /home/browser/work, named in
# the call. Both forms are allowed.
#
# This decides where a command starts, not what it can touch: its arguments
# can name any path the user can read ("cat /data/chrome/..."), and a script
# can `cd`. Use it to keep an agent's work in one place, with a list of
# programs when that has to hold against a hostile agent.
package browserjs.policy

import rego.v1

workdir := "/home/browser/work"

allow_tool_call if {
	input.server == "browser"
	input.tool == "shell_execute"
	is_object(input.arguments)

	# Without cwd the command starts in the home directory: denied.
	cwd := input.arguments.cwd
	is_string(cwd)
	inside_workdir(cwd)
}

# The tool itself refuses a cwd that is not written as the real path of the
# directory ("..", "//", a symbolic link); the check on ".." here means the
# policy does not depend on that.
inside_workdir(cwd) if cwd == workdir

inside_workdir(cwd) if {
	startswith(cwd, concat("", [workdir, "/"]))
	not ".." in split(cwd, "/")
}

allow_tool_call if {
	input.server == "browser"
	input.tool == "shell_process"
}

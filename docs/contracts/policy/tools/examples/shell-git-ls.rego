# Only `git` and `ls`, with any arguments, and nothing else in the session:
# no script form, no extra environment, no browser or desktop tools.
package browserjs.policy

import rego.v1

allowed_programs := {"git", "ls"}

# The fields this policy has an opinion on. A call with any other field
# (`script`, `env`) is denied, whatever its value.
understood_fields := {"argv", "cwd", "stdin", "timeout_ms", "max_output_bytes", "background"}

allow_tool_call if {
	input.server == "browser"
	input.tool == "shell_execute"
	is_object(input.arguments)
	count(object.keys(input.arguments) - understood_fields) == 0
	is_array(input.arguments.argv)

	# The program by name: found on the server's PATH, which a call cannot
	# change. "/tmp/git" and "./git" are other strings, and denied.
	input.arguments.argv[0] in allowed_programs
}

# Reading and ending background commands starts nothing new.
allow_tool_call if {
	input.server == "browser"
	input.tool == "shell_process"
}

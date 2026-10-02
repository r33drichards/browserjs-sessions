# One `git` or `ls` command, with any arguments that are plain words, and
# nothing else in the session.
#
# `cmd` goes to `sh -c`, so "starts with git" is not a rule: the string must
# be shown to be ONE simple command. It is, when it is the program name
# followed by words made only of characters the shell does not interpret,
# separated by single spaces. No quotes, so no argument with a space in it.
package browserjs.policy

import rego.v1

# A word the shell passes on unchanged: none of  space ; & | < > ( ) $ ` \ "
# ' * ? [ ] { } ~ # ! or a newline.
plain_command := `^(git|ls)( [A-Za-z0-9_./:=@%+,-]+)*$`

allow_tool_call if {
	input.server == "exec"
	input.tool == "exec"
	is_object(input.arguments)
	is_string(input.arguments.cmd)
	regex.match(plain_command, input.arguments.cmd)
	is_number(input.arguments.timeout)
	input.arguments.timeout >= 1
	input.arguments.timeout <= 600
}

# Reading the output of a command starts nothing.
allow_tool_call if {
	input.server == "exec"
	input.tool in {"stream_logs", "search_logs"}
}


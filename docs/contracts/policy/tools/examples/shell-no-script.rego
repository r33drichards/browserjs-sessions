# The script form is denied; the argv form and the other tools are allowed.
#
# On its own this is a rule about form, not about what can run: argv
# ["bash", "-c", "..."] is the script form by another name. Its use is that
# every command is then a program and arguments a policy can read; add a list
# of programs (shell-git-ls.rego) to restrict what runs.
package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "browser"
	input.tool in {"browser_execute", "desktop_execute", "shell_process"}
}

allow_tool_call if {
	input.server == "browser"
	input.tool == "shell_execute"
	is_object(input.arguments)
	not "script" in object.keys(input.arguments)
	is_array(input.arguments.argv)
}

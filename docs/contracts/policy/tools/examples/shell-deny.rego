# No commands at all: the browser tool only.
#
# desktop_execute is denied with the shell tools on purpose. It drives the
# mouse and keyboard of a desktop that has a terminal on it, so allowing it
# allows typing commands into that terminal.
package browserjs.policy

import rego.v1

allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
}

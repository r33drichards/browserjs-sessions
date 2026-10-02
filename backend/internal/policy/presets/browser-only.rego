# The browser, with every operation. No desktop control and no shell.
package browserjs.policy

import rego.v1

# Nothing in the browser is restricted here, so the desktop and the shell
# are left out by choice, not by need. To allow one, add a rule for it:
#
#   allow_tool_call if {
#   	input.server == "browser"
#   	input.tool == "desktop_execute"
#   }
allow_tool_call if {
	input.server == "browser"
	input.tool == "browser_execute"
}

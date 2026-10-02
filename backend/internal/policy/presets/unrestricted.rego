# No restrictions: every operation in the browser, full control of the
# desktop, and any shell command.
package browserjs.policy

import rego.v1

# The platform asks a policy only about the tools it knows: browser_execute
# and desktop_execute on server "browser"; exec, stream_logs, search_logs and
# kill on server "exec". This allows all of them, with any arguments.
allow_tool_call := true

package mcp.tools

default allow := false

allow if {
	input.server == "browser"
	input.tool == "browser_execute"
}

# Desktop control (nut.js): the mouse, keyboard, screen and clipboard of the
# display. It can do whatever a person at the VNC view can, including things
# a rule on browser_execute's arguments would have refused, so a policy that
# restricts browser_execute must deny this tool.
allow if {
	input.server == "browser"
	input.tool == "desktop_execute"
}

# Commands on the desktop (mcp-exec, the "exec" server): `exec` starts a
# program with its arguments (no shell), `stream_logs` and `search_logs` read
# its output, `kill` stops it. A command runs as the desktop's user and can
# reach the browser's own control ports on loopback, so, like desktop_execute,
# it can do what a rule on browser_execute's arguments would have refused: a
# policy that restricts browser_execute must deny `exec`. Allowed here because
# this file is only the platform's floor; once a session's policies are
# enforcing, the session's own policy decides, from the program and its
# arguments (docs/contracts/policy/exec-input.md).
allow if {
	input.server == "exec"
	input.tool == "exec"
}

allow if {
	input.server == "exec"
	input.tool == "stream_logs"
}

allow if {
	input.server == "exec"
	input.tool == "search_logs"
}

allow if {
	input.server == "exec"
	input.tool == "kill"
}

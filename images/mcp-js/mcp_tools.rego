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

# Commands on the desktop: shell_execute starts one (`argv`, run directly, or
# `script`, run by bash), shell_process reads and ends the ones started in
# the background. A command runs as the desktop's user and can reach the
# browser's own control ports on loopback, so, like desktop_execute, it can
# do what a rule on browser_execute's arguments would have refused: a policy
# that restricts browser_execute must deny shell_execute, or allow only
# programs that cannot be made to do that. Allowed here because this file is
# only the platform's floor; once a session's policies are enforcing, the
# session's own policy decides, from the tool's arguments
# (docs/contracts/policy/shell-execute-input.md).
allow if {
	input.server == "browser"
	input.tool == "shell_execute"
}

allow if {
	input.server == "browser"
	input.tool == "shell_process"
}

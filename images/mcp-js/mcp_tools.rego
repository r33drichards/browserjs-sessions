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

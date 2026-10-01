package mcp.tools

default allow := false

allow if {
	input.server == "browser"
	input.tool == "browser_execute"
}

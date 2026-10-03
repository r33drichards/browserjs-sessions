package mcp.filesystem

default allow = false

# Only the session data directory, and no path traversal out of it.
in_data(p) if {
	startswith(p, "/data/")
	not contains(p, "..")
}

in_data(p) if p == "/data"

allow if {
	in_data(input.path)
	not input.destination
}

allow if {
	in_data(input.path)
	in_data(input.destination)
}

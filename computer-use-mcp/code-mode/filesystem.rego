package mcp.filesystem

default allow = false

# Only the persistent memory directory, and no path traversal out of it.
in_memory(p) if {
	startswith(p, "/data/memory/")
	not contains(p, "..")
}

in_memory(p) if p == "/data/memory"

allow if {
	in_memory(input.path)
	not input.destination
}

allow if {
	in_memory(input.path)
	in_memory(input.destination)
}

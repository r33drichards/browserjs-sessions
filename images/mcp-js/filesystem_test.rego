package mcp.filesystem

test_data_paths_allowed if {
	every path in ["/data", "/data/memory/notes.md", "/data/scripts/helper.js", "/data/mcp/sessions", "/data/chrome/file"] {
		allow with input as {"path": path}
	}
}

test_outside_paths_denied if {
	every path in ["/", "/etc/passwd", "/database/file", "/data/../etc/passwd", "/data/scripts/../../etc/passwd"] {
		not allow with input as {"path": path}
	}
}

test_copy_and_rename_boundaries if {
	allow with input as {"path": "/data/memory/helper.js", "destination": "/data/scripts/helper.js"}
	not allow with input as {"path": "/data/scripts/helper.js", "destination": "/tmp/helper.js"}
	not allow with input as {"path": "/tmp/helper.js", "destination": "/data/scripts/helper.js"}
}

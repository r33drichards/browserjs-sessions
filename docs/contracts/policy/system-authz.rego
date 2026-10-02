package system.authz

import rego.v1

default allow := false

# A session's mcp-js asking for a decision. It cannot send an identity.
allow if {
	input.method == "POST"
	count(input.path) == 6
	array.slice(input.path, 0, 4) == ["v1", "data", "browserjs", "decision"]
	# No ?explain, ?instrument, ?provenance, ?metrics: they describe the policy.
	object.keys(input.params) == set()
}

# Kubelet probes.
allow if {
	input.method == "GET"
	input.path == ["health"]
}

# The operator, reading what this replica has loaded.
allow if {
	input.identity == opa.runtime().env.OPERATOR_TOKEN
	input.identity != ""
	input.method == "GET"
	input.path == ["v1", "data", "browserjs", "loaded"]
}

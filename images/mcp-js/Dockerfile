# mcp-js for one browserjs session: no JWT check (the backend authenticates
# and is the only client that can reach the pod), browser MCP on localhost,
# artifacts and upload grants on the session disk.
# v0.21.0-rc.3
FROM wholelottahoopla/mcp-js@sha256:1615350e509c06e23bb79ec5d26a839e43c2fbed4675b6987b49088a77dd9bbc
COPY mcp_tools.rego filesystem.rego mcp-servers.json run_js.md instructions.md /etc/mcp/
COPY --chmod=755 start.sh /usr/local/bin/start.sh
ENV MCP_V8_HTTP_PORT=8080 \
    MCP_V8_HEAP_STORE=none \
    MCP_V8_MCP_CONFIG=/etc/mcp/mcp-servers.json \
    MCP_V8_POLICIES_JSON='{"mcp_tools":{"policies":[{"url":"file:///etc/mcp/mcp_tools.rego"}]},"filesystem":{"policies":[{"url":"file:///etc/mcp/filesystem.rego"}]}}' \
    MCP_V8_RUN_JS_DESCRIPTION=@/etc/mcp/run_js.md \
    MCP_V8_INSTRUCTIONS=@/etc/mcp/instructions.md \
    MCP_V8_SESSION_DB_PATH=/data/mcp/sessions
EXPOSE 8080
# The base image's entrypoint is mcp-v8, which dies if the browser MCP on
# 127.0.0.1:8081 is not up yet; start.sh waits for it first.
ENTRYPOINT ["/usr/local/bin/start.sh"]

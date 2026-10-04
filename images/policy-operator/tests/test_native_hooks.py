"""Real pinned MCPJS + native remote hook + Redis. Set MCPJS_BIN to run.

The kind suite also starts the production MCPJS image with the same hook config.
"""
import asyncio
import json
import os
from pathlib import Path
import subprocess
import sys

import pytest
from policy_operator.operator import Operator
from policy_operator.server import make_app
from conftest import free_port

SID = "s-abcde"
ROOT = Path(__file__).resolve().parents[3]


@pytest.mark.skipif(not os.environ.get("MCPJS_BIN"), reason="set MCPJS_BIN to pinned MCPJS v0.21.0-rc.4 binary")
@pytest.mark.parametrize("scenario", ["allowed", "denied", "redis_failure"])
async def test_native_hook_records_before_policy_and_fails_closed(cfg, aiohttp_server, tmp_path, monkeypatch, scenario):
    import redis
    op = Operator(cfg)
    op.ready = True
    await op.webhooks.configure(SID, {"url": "https://example.com/hook", "batch_size": 100, "flush_interval_seconds": 60})
    collector = await aiohttp_server(make_app(op))
    if scenario == "redis_failure":
        def unavailable(_): raise redis.RedisError("Redis unavailable")
        monkeypatch.setattr(op.webhooks.outbox, "ingest", unavailable)
    marker = tmp_path / "executed"
    upstream = tmp_path / "upstream.py"
    upstream.write_text('''import json, sys
from pathlib import Path
for line in sys.stdin:
    message = json.loads(line)
    if "id" not in message: continue
    method = message["method"]
    if method == "initialize":
        result = {"protocolVersion":"2025-03-26", "capabilities":{"tools":{}}, "serverInfo":{"name":"exec","version":"test"}}
    elif method == "tools/list":
        result = {"tools":[{"name":"exec", "description":"test tool", "inputSchema":{"type":"object"}}]}
    elif method == "tools/call":
        Path(sys.argv[1]).write_text("executed")
        result = {"content":[{"type":"text", "text":"stub exec ran"}]}
    else: result = {}
    print(json.dumps({"jsonrpc":"2.0", "id":message["id"], "result":result}), flush=True)
''')
    policy = tmp_path / "policy.rego"
    policy.write_text("package mcp.tools\nallow := " + ("false" if scenario == "denied" else "true") + "\n")
    policies = {"mcp_tools": {"pre": [{"url": str(collector.make_url("/")).rstrip("/"),
                 "policy_path": f"browserjs/hooks/{SID}/mcp_tools/pre"}],
                 "policies": [{"url": policy.as_uri()}]}}
    port = free_port()
    with (tmp_path / "mcpjs.log").open("w") as log:
        process = subprocess.Popen([os.environ["MCPJS_BIN"], "--http-port", str(port),
            "--session-db-path", str(tmp_path / "sessions"), "--policies-json", json.dumps(policies),
            "--mcp-config", json.dumps([{"name":"exec", "transport":"stdio", "command":sys.executable,
                                        "args":[str(upstream), str(marker)]}])], stdout=log, stderr=log)
        try:
            import socket
            for _ in range(100):
                try:
                    with socket.create_connection(("127.0.0.1", port), timeout=.1): break
                except OSError: await asyncio.sleep(.1)
            call = await asyncio.to_thread(subprocess.run, [sys.executable, str(ROOT / "test/policy/mcp_call.py"),
                                f"http://127.0.0.1:{port}", "ls"],
                                env={**os.environ, "SERVER":"exec"}, capture_output=True, text=True, timeout=20)
            assert call.returncode == 0, call.stderr + (tmp_path / "mcpjs.log").read_text()
            result = json.loads(call.stdout)
            assert result["outcome"] == ("ran" if scenario == "allowed" else "denied"), result
            assert marker.exists() == (scenario == "allowed")
            groups = op.webhooks.outbox.groups()
            if scenario == "redis_failure":
                assert not groups
            else:
                assert len(groups) == 1
                event = op.webhooks.outbox.events(groups[0], 1, 10000)[0][1]
                assert event["stage"] == "attempt" and event["server"] == "exec" and event["tool"] == "exec"
                assert "allowed" not in event
        finally:
            process.terminate()
            await asyncio.to_thread(process.wait, timeout=10)
            await op.close()

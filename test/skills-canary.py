#!/usr/bin/env python3
"""Verify the targeted user's skills through a temporary ordinary session."""
import base64
import hashlib
import json
import os
import re
import sys
import time

import canary


def verify(mcp):
    result = mcp.rpc("initialize", {
        "protocolVersion": "2025-03-26",
        "capabilities": {"extensions": {"io.modelcontextprotocol/skills": {}}},
        "clientInfo": {"name": "skills-canary", "version": "1"},
    })
    canary.expect("io.modelcontextprotocol/skills" in result.get("capabilities", {}).get("extensions", {}),
                  "server did not advertise skills")
    mcp.post({"jsonrpc": "2.0", "method": "notifications/initialized"}, 30)
    skills = mcp.rpc("skills/list", {})["skills"]
    skill = next(s for s in skills if s["frontmatter"]["name"] == "project-documentation")
    manifest = mcp.rpc("skills/get", {"uri": skill["uri"]})["skill"]
    for path in ("SKILL.md", "INDEX.md", "references/docs/mcp-skills.md"):
        uri = "skill://project-documentation/" + path
        entry = next(r for r in manifest["resources"] if r["uri"] == uri)
        contents = mcp.rpc("resources/read", {"uri": uri})["contents"]
        canary.expect(len(contents) == 1 and contents[0]["uri"] == uri, "unexpected resource contents")
        content = contents[0]
        data = content["text"].encode() if "text" in content else base64.b64decode(content["blob"], validate=True)
        canary.expect(len(data) == entry["size"] and "sha256:" + hashlib.sha256(data).hexdigest() == entry["digest"],
                      "resource hash or size mismatch")
    print("Verified native skills discovery, manifest, entrypoint, index and documentation hashes.")


def main():
    token = canary.TOKEN
    match = re.fullmatch(r"bjs_([^_]+)_.+", token)
    if not match:
        raise RuntimeError("CANARY_API_TOKEN must be an API token")
    basic = base64.b64encode((match[1] + ":" + token).encode()).decode()
    status, _, raw = canary.http(
        "POST", canary.API + "/oauth/token", b"grant_type=client_credentials",
        {"Authorization": "Basic " + basic, "Content-Type": "application/x-www-form-urlencoded"},
    )
    canary.expect(status == 200, "token exchange failed")
    canary.ACCESS["token"] = json.loads(raw)["access_token"]
    status, me = canary.api("GET", "/v1/me")
    canary.expect(status == 200 and me.get("email", "").lower() == os.environ["EXPECTED_EMAIL"].lower(),
                  "canary token must belong to the rollout target")
    if "--identity-only" in sys.argv:
        print("Verified canary token belongs to the rollout target.")
        return
    status, session = canary.api("POST", "/v1/sessions", {
        "name": "release-canary-skills-" + time.strftime("%Y%m%d-%H%M%S", time.gmtime()),
    })
    canary.expect(status == 201, "session creation failed")
    sid = session["id"]
    try:
        canary.wait_state(sid, "running", 420)
        mcp = canary.MCP(sid)
        verify(mcp)
    finally:
        status, _ = canary.api("DELETE", "/v1/sessions/" + sid)
        canary.expect(status in (200, 202, 204), "temporary session cleanup failed")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(canary.scrub(error), file=sys.stderr)
        sys.exit(1)

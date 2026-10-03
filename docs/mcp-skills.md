# Repository documentation as MCP skills

The optional skills image serves `project-documentation` through SEP-2640.
It bundles this repository's `docs/`, `site/reference/`, and root README.
An index helps a client select pages without reading the whole corpus into
the prompt. The server advertises file sizes and SHA-256 digests; Pi's
`codex/sep-2640-skills-v1.0.1` branch verifies them and loads files lazily.

The image compiles `r33drichards/mcp-js` commit
`df7ff14823861941bfe1d9f245a39c045a243e86` from the
`codex/user-supplied-skills` branch (PR #272). It uses that branch's legacy
`initialize` handshake; do not set Pi's modern `protocolVersion` for it.
Changing the pin requires rebuilding and selecting a new image digest.

Build from the repository root:

```sh
docker build -f images/mcp-js/Dockerfile.skills -t mcp-js:skills .
```

The images workflow also builds `mcp-js-skills`. On main it publishes to the
existing `<registry>/mcp-js` repository with `skills-<commit>` and
`skills-main` tags. The normal image's tags and session blueprint remain
unchanged. Use the published digest, never the moving tag, for rollout.

## Roll out to one user

The backend evaluates OpenFeature's `mcp-skills` boolean flag at session
creation using the authenticated user's verified email as `targetingKey`.
The initial provider is the SDK's in-memory provider with a deployment
allowlist. No separate feature-flag service is required. A different
OpenFeature provider can replace it through `NewSkillsWithProvider`.

It defaults off. `MCP_SKILLS_EMAILS` selects recipients and
`MCP_SKILLS_IMAGE_DIGEST` identifies the skills-enabled image. Both are
optional ConfigMap references in the backend manifest. Errors default off,
and requests cannot supply the identity used for flag evaluation.

After the backend containing this integration and the skills image have
been built and deployed:

```sh
hack/skills-stage.sh on sha256:<64-lowercase-hex-digits> your-verified-email@example.com
```

This creates/updates ConfigMap `mcp-skills` and restarts the backend template
through the existing rollout mechanism. Check the backend rollout succeeds
before creating a session. The targeted user's new session starts cold with
the skills image; it bypasses the stable warm pool. Other users use the
existing image and warm pool. Explicit administrator canary image selections
take precedence. Turning the flag off affects future creates only:

```sh
hack/skills-stage.sh off
```

Existing sessions retain their image digest, even across stop/resume.
Create a new session to try the feature; do not delete a session just to
switch images unless its owner intends to discard it.

## Verify through Pi

Point the Pi launcher at the new session's MCP URL. `/mcp` should show a
connected server. Ask Pi to list remote skills, then load the manifest:

```text
/mcp-skill computeruse skill://project-documentation/SKILL.md Explain how sessions deploy
```

Pi asks for consent, verifies `SKILL.md`, and reads `INDEX.md` or individual
supporting pages only when needed. The snapshot is read-only and tied to
the image. Edits to the repository require a new image; SEP-2640 itself
does not supply a write API.

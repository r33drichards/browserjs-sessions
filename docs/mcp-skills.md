# Repository documentation as MCP skills

The optional skills image serves one SEP-2640 skill per Markdown page in
`docs/`, `site/reference/`, and the root README. Nested contract and design-plan
pages are individually discoverable too. For example:

- `docs-api-tokens` — the API tokens page.
- `docs-contracts-billing-testing` — the billing testing contract.
- `reference-mcp` — the MCP endpoint and `run_js` reference.
- `project-readme` — the root README.

Each skill's `SKILL.md` contains that page's full text. Its manifest includes only
that entrypoint and the local images or attachments linked from the page. Links
to other documentation pages use their separate `skill://` entrypoints, so the
agent can load only the pages relevant to its task. Historical plans are labelled
in their descriptions. There is no top-level `project-documentation` umbrella.

Names derive from source paths, retaining nested topics in the name. The build
checks name collisions and SEP-2640 file/size limits. Pi's
`codex/sep-2640-skills-v1.0.1` branch verifies resource hashes and loads skills
lazily.

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
/mcp-skill computeruse skill://docs-releases/SKILL.md Explain how sessions deploy
```

Pi asks for consent and verifies that page's `SKILL.md`. Linked page skills
and supporting assets are loaded only when needed. The snapshot is read-only and tied to
the image. Edits to the repository require a new image; SEP-2640 itself
does not supply a write API.

For deployment without local Google Cloud credentials, run the **Skills rollout**
GitHub Actions workflow on `main` after the backend release has deployed. Select
`on`, provide the published immutable skills image digest and one verified email,
and set `confirm` to `deploy`. It uses the existing production workload identity
and waits for the backend rollout. Select `off` to disable future skills sessions.
The workflow shares the deployment concurrency group so cluster changes serialize.
When `CANARY_API_TOKEN` is configured, the activation workflow verifies that it
belongs to the selected user before changing the flag. After rollout it creates
and deletes a temporary ordinary session and verifies skills discovery and
documentation resource hashes. Without the token, it reports that live session
verification was skipped; backend rollout health is still checked. If activation
or verification fails, it disables skills for future sessions.

# Session policies: the UI

Track D of [the session-policies plan](plans/2026-10-02-session-policies-tracks.md).
Built against [`contracts/policy/backend-api.yaml`](contracts/policy/backend-api.yaml);
until the backend track is merged it runs against a mock of that contract.

## Pages

| Route | What | Cloudscape pattern |
|---|---|---|
| `/` | Sessions, with a Policy column and an "as code" tag | table view (as before) |
| `/sessions/create` | Name (pet-name placeholder) and the policy | [single page create](https://cloudscape.design/patterns/resource-management/create/single-page-create/); the policy is a [sub-resource create](https://cloudscape.design/patterns/resource-management/create/sub-resource-create/): embedded radios for the ready-made choices, a split panel for writing one |
| `/sessions/:id` | Title row, then Browser and Policy tabs (`?tab=policy`) | [details page with tabs](https://cloudscape.design/patterns/resource-management/details/details-page-with-tabs/) |
| `/sessions/:id/policy/edit` | The editor; read-only when managed as code | [page edit](https://cloudscape.design/patterns/resource-management/edit/page-edit/) |
| `/tokens`, `/tokens/create` | API tokens; the secret shown once | table view, single page create |

Leaving a page with changes raises the Leave page modal of
[communicating unsaved changes](https://cloudscape.design/patterns/general/unsaved-changes/)
(`web/src/useUnsavedChanges.tsx`), and `beforeunload` for the browser's own
navigation. With nothing changed there is no modal.

## Where the design's pattern table was adjusted

The design read the pattern pages through a summary. Read directly, they say
a few things it did not, and the UI follows the pages:

- **Page edit**: the button is "Save [object]" ("Save policy", not "Save").
  Saving with no changes returns to the page the edit started from with an
  info flashbar, "No changes were made to the policy."
- **Unsaved changes**: no modal "when there is no risk of data loss". Cancel
  on an untouched form leaves at once; the design had "Cancel with anything
  entered", which is the same thing said the other way.
- **Single page create / page edit**: "Don't disable the primary button".
  Create and Save are always enabled; what is missing is said on the field.
- **Sub-resource create**: "Don't use a modal for sub-resource creation".
  Writing a policy is in the split panel; modals are used only for
  confirmations and for the two-field management setting.
- **Code editor** (the component's usage page): "Don't use the code editor
  component to display non-editable code snippets". The read-only views (the
  Policy tab, the generated Rego, a policy managed as code) are plain code
  blocks, not Monaco. The design had Monaco read-only for "View source". This
  also keeps Monaco off the session page. Its other guidance is followed too:
  a status bar (language, cursor, error and warning counts), a problems pane,
  a loading state and an error state for the editor, and the editor is never
  in a modal. There is no preferences modal.
- **Details page with tabs**: the summary container is optional. The session's
  title row (state, created, Copy MCP URL: PR #41 moved them there and removed
  the Details box) is the summary that stays in view; the policy's one-line
  state is added to it rather than bringing a box back.

## Managed as code

Per session's policy, as in the design (section 2.3) and like Tailscale's
policy file when it is managed by GitOps:

- The list and the session's title row carry an "as code" tag.
- The Policy tab shows an alert, "This policy is managed as code", with the
  link (`management.managed_url`) and "Manage here instead". Edit, Reset and
  Copy from session are absent; "View source" opens the edit route read-only.
- `/sessions/:id/policy/edit` opened directly shows that read-only view (the
  source, the Rego in force, and Test, which saves nothing), not an error.
- "Manage here instead" opens the management modal on "This editor"; saving
  it (`PUT …/policy/management`) returns the write actions. There is no
  "edit anyway".
- A save refused with 409 because the mode changed while the editor was open
  says so, shows the link from the answer, and offers Reload.

## When the backend has the feature off

The backend ships policies behind `POLICY_OPERATOR_URL`. With it unset the
sessions carry no `policy` and the policy endpoints are not routed (404).
The UI then shows nothing of the feature and no errors: no Policy column, no
tabs (the browser is the whole session page), a create page with the name
alone that sends `{"name": …}` as before, and no "API tokens" link unless
`/tokens` answers. `ifAvailable` in `web/src/policyApi.ts` is the one place
that decides; a signed-out proxy is still treated as signed out.

## The editor

Monaco from the npm package (`monaco-editor`), never a CDN: the editor core,
the JSON language service and their two workers are built into the app's own
assets (`web/src/policy/monaco.ts`). It is a lazy chunk, fetched only when a
policy is edited. JSON problems come from the schema (`/policy-schema.json`)
in the browser; everything else from `/policies/validate`, 400 ms after the
last keystroke, shown as markers and in the problems pane. Rego highlighting
is a Monarch grammar (`web/src/policy/rego.ts`).

## Developing without a backend

```
cd web
nix develop -c npm run dev:mock        # every feature on
MOCK=off nix develop -c npx vite       # a backend with the feature off
```

`web/mock/backend.ts` answers `/api` as `backend-api.yaml` says, with the
contract's examples as presets and its schema, and seeds a session in each
policy state: `research` (editor, in force), `ci-runner` (managed as code),
`just-saved` (loading), `broken` (does not compile), `from-before`
(unsupported). It does not compile Rego: see the comment at its top. The
component tests run against the same mock.

## Screenshots

From the mock, in headless Chrome: [`policy-ui/`](policy-ui/).

| | |
|---|---|
| ![list](policy-ui/01-list.png) | ![create](policy-ui/02-create.png) |
| ![split panel](policy-ui/04-create-panel-errors.png) | ![created](policy-ui/17-created-flash.png) |
| ![policy tab](policy-ui/06-detail-policy.png) | ![managed as code](policy-ui/07-detail-policy-iac.png) |
| ![management](policy-ui/08-management-modal.png) | ![edit](policy-ui/11-edit.png) |
| ![rego](policy-ui/13-edit-rego.png) | ![read-only](policy-ui/14-edit-readonly.png) |
| ![leave](policy-ui/19-leave-modal.png) | ![token](policy-ui/16-token-created.png) |

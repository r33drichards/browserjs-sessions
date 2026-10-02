// Cloudscape's page edit, for the session's policy. A policy managed as code
// gets the same layout read-only: the source, the policy in force and Test,
// with the link to where it is edited in place of the buttons.
import Alert from "@cloudscape-design/components/alert"
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import ButtonDropdown from "@cloudscape-design/components/button-dropdown"
import Container from "@cloudscape-design/components/container"
import Form from "@cloudscape-design/components/form"
import Header from "@cloudscape-design/components/header"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { useCallback, useEffect, useState } from "react"
import { useNavigate } from "react-router-dom"
import { ApiError, isSessionId } from "../api"
import { signedOutHandled } from "../auth/signedOut"
import type { Problems } from "../components/PolicyWorkbench"
import { PolicyWorkbench } from "../components/PolicyWorkbench"
import type { Policy, PolicySession, PolicySource, Preset } from "../policyApi"
import { PolicyApiError, ifAvailable, isManagedAsCode, policyApi } from "../policyApi"
import type { Flash } from "../shell"
import { Shell, api } from "../shell"
import { useUnsavedChanges } from "../useUnsavedChanges"

type Failure = null | { kind: "changed" } | { kind: "mode"; message: string; managedUrl?: string } | { kind: "other"; message: string }

// Mounted with key={id}.
export function EditPolicy({ id }: { id: string }) {
  const navigate = useNavigate()
  const [session, setSession] = useState<PolicySession | null>(null)
  const [policy, setPolicy] = useState<Policy | null>(null) // as it was when the edit started
  const [draft, setDraft] = useState<PolicySource | null>(null)
  const [missing, setMissing] = useState(() => !isSessionId(id))
  const [loadError, setLoadError] = useState("")
  const [failure, setFailure] = useState<Failure>(null)
  const [refused, setRefused] = useState<Problems | null>(null)
  const [busy, setBusy] = useState(false)
  const [others, setOthers] = useState<PolicySession[]>([])
  const [presets, setPresets] = useState<Preset[] | null>(null)

  const load = useCallback(async () => {
    setLoadError("")
    try {
      const s: PolicySession = await api.getSession(id)
      setSession(s)
      // A session from before policies, or a deployment without them, has none to edit.
      if (!s.policy || s.policy.state === "unsupported") return
      const p = await policyApi.getPolicy(id)
      setPolicy(p)
      setDraft({ kind: p.kind ?? "rego", source: p.source ?? "" })
      setFailure(null)
      setRefused(null)
    } catch (e) {
      if (signedOutHandled(e)) return
      if (e instanceof ApiError && e.status === 404) setMissing(true)
      else setLoadError(String(e instanceof Error ? e.message : e))
    }
  }, [id])

  useEffect(() => {
    if (!missing) void load()
  }, [load, missing])

  useEffect(() => {
    api
      .listSessions()
      .then((list: PolicySession[]) => setOthers(list.filter(s => s.id !== id && s.policy && s.policy.state !== "unsupported")))
      .catch(signedOutHandled)
    // The ready-made policies, to start over from one. The editor works without them.
    ifAvailable(policyApi.presets()).then(setPresets).catch(signedOutHandled)
  }, [id])

  const readOnly = isManagedAsCode(policy)
  const changed = !!policy && !!draft && (draft.kind !== policy.kind || draft.source !== (policy.source ?? ""))
  const unsaved = useUnsavedChanges(changed && !readOnly)
  const back = `/sessions/${id}?tab=policy`

  function leave(flash: Flash) {
    unsaved.markSaved()
    navigate(back, { state: { flash } })
  }

  async function save() {
    if (busy || !policy || !draft) return
    // Nothing to send: back to where the edit started, saying so.
    if (!changed) return leave({ type: "info", content: "No changes were made to the policy." })
    setBusy(true)
    setFailure(null)
    setRefused(null)
    try {
      const saved = await policyApi.putPolicy(id, draft, policy.version)
      leave(
        saved.inForce
          ? { type: "success", content: `Policy saved and in force (v${saved.policy.version})` }
          : { type: "info", content: "Policy saved; loading" },
      )
    } catch (e) {
      if (signedOutHandled(e)) return
      const message = String(e instanceof Error ? e.message : e)
      if (e instanceof PolicyApiError && e.status === 412) setFailure({ kind: "changed" })
      else if (e instanceof PolicyApiError && e.status === 409) setFailure({ kind: "mode", message, managedUrl: e.managedUrl })
      else {
        if (e instanceof PolicyApiError && e.status === 422) setRefused({ errors: e.errors, warnings: e.warnings })
        setFailure({ kind: "other", message })
      }
    } finally {
      setBusy(false)
    }
  }

  async function copyFrom(sessionId: string) {
    try {
      const from = await policyApi.getPolicy(sessionId)
      if (from.kind && from.source) setDraft({ kind: from.kind, source: from.source })
    } catch (e) {
      if (!signedOutHandled(e)) setFailure({ kind: "other", message: String(e instanceof Error ? e.message : e) })
    }
  }

  const title = readOnly ? "View policy" : "Edit policy"
  const crumbs = [
    { text: session?.name ?? id, href: `/sessions/${id}` },
    { text: title, href: `/sessions/${id}/policy/edit` },
  ]

  if (missing) {
    return (
      <Shell>
        <Box padding="l">This session doesn&apos;t exist, or isn&apos;t yours.</Box>
      </Shell>
    )
  }
  if (session && (!session.policy || session.policy.state === "unsupported")) {
    return (
      <Shell breadcrumbs={crumbs}>
        <Alert type="info" header="This session can't have a policy">
          {session.policy
            ? "It was created before session policies existed. To use a policy, create a new session."
            : "Session policies aren't available on this deployment."}
        </Alert>
      </Shell>
    )
  }
  if (!session || !policy || !draft) return <Shell breadcrumbs={crumbs}>{loadError || "Loading…"}</Shell>

  const workbench = (
    <PolicyWorkbench
      draft={draft}
      onChange={d => {
        setDraft(d)
        setRefused(null)
      }}
      readOnly={readOnly}
      regoInForce={policy.rego}
      refused={refused}
      presets={presets}
      tools={
        others.length > 0 ? (
          <ButtonDropdown
            items={others.map(s => ({ id: s.id, text: s.name }))}
            onItemClick={e => void copyFrom(e.detail.id)}
          >
            Copy from session
          </ButtonDropdown>
        ) : undefined
      }
    />
  )

  const alerts = (
    <>
      {failure?.kind === "changed" && (
        <Alert type="warning" header="This policy changed since you opened it" action={<Button onClick={() => void load()}>Reload</Button>}>
          Nothing was saved. Reload shows the policy as it is now; the changes in the editor are then lost, so copy what
          you want to keep.
        </Alert>
      )}
      {failure?.kind === "mode" && (
        <Alert type="warning" header="This policy is now managed as code" action={<Button onClick={() => void load()}>Reload</Button>}>
          Nothing was saved: {failure.message}.{" "}
          {failure.managedUrl && (
            <>
              Edit it at{" "}
              <a href={failure.managedUrl} target="_blank" rel="noopener noreferrer">
                {failure.managedUrl}
              </a>
              .
            </>
          )}
        </Alert>
      )}
      {failure?.kind === "other" && (
        <Alert type="error" header={refused ? "The policy does not validate. Nothing was saved." : "The policy was not saved"}>
          {failure.message}
        </Alert>
      )}
    </>
  )

  if (readOnly) {
    return (
      <Shell breadcrumbs={crumbs}>
        <SpaceBetween size="l">
          <Header variant="h1" actions={<Button onClick={() => navigate(back)}>Back to the session</Button>}>
            View policy
          </Header>
          <Alert type="info" header="Read-only: managed as code">
            {policy.management?.managed_url ? (
              <>
                Edit it at{" "}
                <a href={policy.management.managed_url} target="_blank" rel="noopener noreferrer">
                  {policy.management.managed_url}
                </a>
                .{" "}
              </>
            ) : null}
            To edit it here, choose Manage here instead on the session&apos;s Policy tab.
          </Alert>
          <Container header={<Header variant="h2">Policy</Header>}>{workbench}</Container>
        </SpaceBetween>
      </Shell>
    )
  }

  return (
    <Shell breadcrumbs={crumbs}>
      <form
        onSubmit={e => {
          e.preventDefault()
          void save()
        }}
      >
        <Form
          header={
            <Header variant="h1" description={`The policy of ${session.name}. Saving restarts nothing and interrupts no running call.`}>
              Edit policy
            </Header>
          }
          actions={
            <SpaceBetween direction="horizontal" size="xs">
              <Button variant="link" formAction="none" onClick={() => navigate(back)}>
                Cancel
              </Button>
              <Button variant="primary" loading={busy} formAction="submit">
                Save policy
              </Button>
            </SpaceBetween>
          }
        >
          <SpaceBetween size="l">
            {alerts}
            <Container header={<Header variant="h2">Policy</Header>}>{workbench}</Container>
          </SpaceBetween>
        </Form>
      </form>
      {unsaved.modal}
    </Shell>
  )
}

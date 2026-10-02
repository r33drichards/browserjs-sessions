// The Policy tab of a session's page: the policy as it is, read-only. Edit
// goes to its own page; a policy managed as code has no write actions here,
// only the link to where it is edited and the way back.
import Alert from "@cloudscape-design/components/alert"
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import ColumnLayout from "@cloudscape-design/components/column-layout"
import Container from "@cloudscape-design/components/container"
import FormField from "@cloudscape-design/components/form-field"
import Header from "@cloudscape-design/components/header"
import Modal from "@cloudscape-design/components/modal"
import Select from "@cloudscape-design/components/select"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { useCallback, useEffect, useState } from "react"
import { useNavigate } from "react-router-dom"
import { ApiError } from "../api"
import { signedOutHandled } from "../auth/signedOut"
import { problemLine } from "../policy/markers"
import type { ManagementMode, Policy, PolicySession, PolicySummary } from "../policyApi"
import { KIND_LABEL, isManagedAsCode, isUnrestricted, policyApi, policyStatus, updatedByLabel } from "../policyApi"
import { api } from "../shell"
import { usePolling } from "../usePolling"
import { ManagementModal } from "./ManagementModal"
import { CodeView } from "./PolicyEditor"

interface Props {
  sessionId: string
  sessionName: string
  summary: PolicySummary // from the session, which the page already polls
}

type Dialog = null | "reset" | "copy" | { management: ManagementMode | undefined }

export function PolicyTab({ sessionId, sessionName, summary }: Props) {
  const navigate = useNavigate()
  const [policy, setPolicy] = useState<Policy | null>(null)
  const [error, setError] = useState("") // last load failure; cleared by the next good one
  const [notice, setNotice] = useState("") // what the last action did
  const [actionError, setActionError] = useState("")
  const [dialog, setDialog] = useState<Dialog>(null)
  const [busy, setBusy] = useState(false)
  const [others, setOthers] = useState<PolicySession[]>([]) // sessions a policy can be copied from
  const [copyFrom, setCopyFrom] = useState("")
  const unsupported = summary.state === "unsupported"

  const load = useCallback(() => {
    policyApi
      .getPolicy(sessionId)
      .then(p => {
        setPolicy(p)
        setError("")
      })
      .catch(e => {
        if (!signedOutHandled(e)) setError(String(e instanceof Error ? e.message : e))
      })
  }, [sessionId])

  // A session from before policies has no policy object to ask for.
  usePolling(load, !unsupported)

  useEffect(() => {
    if (dialog !== "copy") return
    api
      .listSessions()
      .then((list: PolicySession[]) =>
        setOthers(list.filter(s => s.id !== sessionId && s.policy && s.policy.state !== "unsupported")),
      )
      .catch(signedOutHandled)
  }, [dialog, sessionId])

  if (unsupported) {
    return (
      <Alert type="info" header="This session can't have a policy">
        It was created before session policies existed, so an agent connected to it over MCP is not restricted. To use a
        policy, create a new session.
      </Alert>
    )
  }
  if (!policy) return <Box padding="l">{error ? `Couldn't load the policy: ${error}` : "Loading the policy"}</Box>

  const asCode = isManagedAsCode(policy)
  const editPath = `/sessions/${sessionId}/policy/edit`

  // Runs a write and shows what came of it; a refusal stays until the next action.
  async function act(fn: () => Promise<{ policy: Policy; inForce: boolean }>, done: string) {
    if (busy) return
    setBusy(true)
    setActionError("")
    setNotice("")
    try {
      const saved = await fn()
      setPolicy(saved.policy)
      setNotice(saved.inForce ? `${done} and in force (v${saved.policy.version}).` : `${done}; loading.`)
      setDialog(null)
    } catch (e) {
      if (signedOutHandled(e)) return
      setActionError(String(e instanceof Error ? e.message : e))
      setDialog(null)
      load() // a 409 means the mode changed under us: show what is true now
    } finally {
      setBusy(false)
    }
  }

  async function copyPolicy() {
    const from = await policyApi.getPolicy(copyFrom)
    if (!from.kind || !from.source) throw new ApiError(409, "That session has no policy to copy.")
    return policyApi.putPolicy(sessionId, { kind: from.kind, source: from.source }, policy?.version)
  }

  const copyOptions = others.map(s => ({ value: s.id, label: s.name }))
  const fileName = policy.kind === "rego" ? "policy.rego" : "policy.json"

  return (
    <SpaceBetween size="m">
      {asCode && (
        <Alert
          type="info"
          header="This policy is managed as code"
          action={<Button onClick={() => setDialog({ management: "editor" })}>Manage here instead</Button>}
        >
          {policy.management?.managed_url ? (
            <>
              Edit it at{" "}
              <a href={policy.management.managed_url} target="_blank" rel="noopener noreferrer">
                {policy.management.managed_url}
              </a>
              .{" "}
            </>
          ) : null}
          It can&apos;t be changed here: changes made here would be overwritten.
        </Alert>
      )}
      {policy.state === "invalid" && (
        <Alert
          type="error"
          header="This policy does not compile"
          action={asCode ? undefined : <Button onClick={() => navigate(editPath)}>Edit policy</Button>}
        >
          {policy.hash ? "The previous policy is still in force." : "Until it does, every call is refused."}
          <ul className="wf-problems">
            {(policy.errors ?? []).map((d, i) => (
              <li key={i}>{problemLine(d)}</li>
            ))}
          </ul>
        </Alert>
      )}
      {notice ? (
        <Alert type="success" dismissible onDismiss={() => setNotice("")}>
          {notice}
        </Alert>
      ) : null}
      {actionError ? (
        <Alert type="error" dismissible onDismiss={() => setActionError("")}>
          {actionError}
        </Alert>
      ) : null}
      {error ? <Box>⚠ {error}</Box> : null}

      <Container
        header={
          <Header
            variant="h2"
            description="What an agent connected over MCP may ask this browser to do. It does not restrict you at the screen."
            actions={
              asCode ? (
                <Button onClick={() => navigate(editPath)}>View source</Button>
              ) : (
                <SpaceBetween direction="horizontal" size="xs">
                  <Button onClick={() => setDialog("copy")}>Copy from session</Button>
                  <Button onClick={() => setDialog("reset")}>Reset</Button>
                  <Button variant="primary" onClick={() => navigate(editPath)}>
                    Edit
                  </Button>
                </SpaceBetween>
              )
            }
          >
            Policy
          </Header>
        }
      >
        <SpaceBetween size="m">
          <ColumnLayout columns={4} variant="text-grid">
            <div>
              <Box variant="awsui-key-label">Kind</Box>
              {policy.kind ? KIND_LABEL[policy.kind] : "-"}
            </div>
            <div>
              <Box variant="awsui-key-label">Version</Box>
              {policy.version ?? "-"}
            </div>
            <div>
              <Box variant="awsui-key-label">Status</Box>
              <span data-testid="policy-status">
                {policy.state === "loading" && <span className="wf-spinner wf-spinner-small" aria-hidden="true" />}{" "}
                {policyStatus(policy)}
              </span>
            </div>
            <div>
              <Box variant="awsui-key-label">Hash</Box>
              <span className="wf-mono" title={policy.hash}>
                {policy.hash ? `${policy.hash.replace(/^sha256:/, "").slice(0, 12)}…` : "-"}
              </span>
            </div>
            <div>
              <Box variant="awsui-key-label">Managed in</Box>
              {asCode ? "code" : "this editor"}{" "}
              {!asCode && (
                <Button variant="inline-link" onClick={() => setDialog({ management: undefined })}>
                  Change
                </Button>
              )}
            </div>
            <div>
              <Box variant="awsui-key-label">Last saved</Box>
              {policy.updated ? `${new Date(policy.updated).toLocaleString()} ${updatedByLabel(policy.updated_by)}` : "-"}
            </div>
          </ColumnLayout>

          {isUnrestricted(policy) && <Box>No restrictions: an agent may use every browser operation.</Box>}

          <ColumnLayout columns={policy.kind === "json" ? 2 : 1}>
            <div>
              <Box variant="awsui-key-label">{fileName} (read-only)</Box>
              <CodeView code={policy.source ?? ""} label={`${fileName}, read-only`} height={320} />
            </div>
            {policy.kind === "json" && (
              <div>
                <Box variant="awsui-key-label">Generated Rego (read-only)</Box>
                <CodeView code={policy.rego ?? ""} label="Generated Rego, read-only" height={320} />
              </div>
            )}
          </ColumnLayout>
        </SpaceBetween>
      </Container>

      {typeof dialog === "object" && dialog !== null && (
        <ManagementModal
          sessionId={sessionId}
          current={policy.management ?? { mode: "editor" }}
          initialMode={dialog.management}
          onDismiss={() => setDialog(null)}
          onChanged={p => {
            setPolicy(p)
            setDialog(null)
            setActionError("")
            setNotice(isManagedAsCode(p) ? "This policy is now managed as code." : "This policy is now managed in this editor.")
          }}
        />
      )}

      {dialog === "reset" && (
        <Modal
          visible
          onDismiss={() => setDialog(null)}
          header="Reset policy"
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="xs">
                <Button variant="link" onClick={() => setDialog(null)}>
                  Cancel
                </Button>
                <Button variant="primary" loading={busy} onClick={() => act(() => policyApi.resetPolicy(sessionId), "Policy reset")}>
                  Reset policy
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          The policy of {sessionName} is replaced by one with no restrictions: an agent may then use every browser
          operation. The policy it has now is not kept.
        </Modal>
      )}

      {dialog === "copy" && (
        <Modal
          visible
          onDismiss={() => setDialog(null)}
          header="Copy policy from a session"
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="xs">
                <Button variant="link" onClick={() => setDialog(null)}>
                  Cancel
                </Button>
                <Button variant="primary" loading={busy} disabled={!copyFrom} onClick={() => act(copyPolicy, "Policy copied")}>
                  Copy policy
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          <FormField
            label="Session to copy from"
            description={`The policy of ${sessionName} is replaced by a copy. The two are changed separately afterwards.`}
          >
            <Select
              selectedOption={copyOptions.find(o => o.value === copyFrom) ?? null}
              options={copyOptions}
              placeholder="Choose a session"
              empty="No other session has a policy"
              onChange={e => setCopyFrom(e.detail.selectedOption.value ?? "")}
            />
          </FormField>
        </Modal>
      )}
    </SpaceBetween>
  )
}

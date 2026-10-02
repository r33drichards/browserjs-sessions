// Cloudscape's single page create. The policy is a sub-resource of the
// session: the ready-made choices are embedded in the form, and writing one
// happens in a split panel that keeps the form in view.
import Alert from "@cloudscape-design/components/alert"
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import Container from "@cloudscape-design/components/container"
import Form from "@cloudscape-design/components/form"
import FormField from "@cloudscape-design/components/form-field"
import Header from "@cloudscape-design/components/header"
import Input from "@cloudscape-design/components/input"
import Modal from "@cloudscape-design/components/modal"
import RadioGroup from "@cloudscape-design/components/radio-group"
import Select from "@cloudscape-design/components/select"
import SpaceBetween from "@cloudscape-design/components/space-between"
import SplitPanel from "@cloudscape-design/components/split-panel"
import { useEffect, useState } from "react"
import { useNavigate } from "react-router-dom"
import { ApiError } from "../api"
import { signedOutHandled } from "../auth/signedOut"
import { useCreateGate } from "../billing/SessionBilling"
import { PanelShell } from "../components/PanelShell"
import type { Problems } from "../components/PolicyWorkbench"
import { PolicyWorkbench } from "../components/PolicyWorkbench"
import { problemLine } from "../policy/markers"
import { REGO_TEMPLATE } from "../policy/rego"
import { petname } from "../petname"
import type { PolicyInput, PolicySession, PolicySource, Preset } from "../policyApi"
import { KIND_LABEL, PolicyApiError, ifAvailable, managedUrlError, policyApi } from "../policyApi"
import type { Flash } from "../shell"
import { Shell, api } from "../shell"
import { useUnsavedChanges } from "../useUnsavedChanges"

const COPY = "copy"
const CUSTOM = "custom"
const IAC = "iac"
const presetChoice = (id: string) => `preset:${id}`

export interface PolicyChoice {
  choice: string // presetChoice(id), COPY, CUSTOM or IAC
  presets: Preset[] // `unrestricted` first
  copied?: PolicySource // the source of the session picked under COPY
  custom?: PolicySource // what the split panel handed over
  managedUrl: string
}

// The `policy` of the create request for each choice on the form. Behind
// every choice is the same thing: a policy for the new session.
export function policyForChoice({ choice, presets, copied, custom, managedUrl }: PolicyChoice): PolicyInput {
  const pick = ({ kind, source }: PolicySource): PolicyInput => ({ kind, source })
  const unrestricted = presets.find(p => p.id === "unrestricted") ?? presets[0]
  if (choice === COPY && copied) return pick(copied)
  if (choice === CUSTOM && custom) return pick(custom)
  // Managed as code: no restrictions until the policy arrives by API token.
  if (choice === IAC) return { ...pick(unrestricted), management: { mode: "iac", managed_url: managedUrl.trim() } }
  return pick(presets.find(p => presetChoice(p.id) === choice) ?? unrestricted)
}

const lineCount = (source: string) => source.replace(/\n$/, "").split("\n").length

const NEW_DRAFT: PolicySource = { kind: "rego", source: REGO_TEMPLATE }
const same = (a: PolicySource, b: PolicySource) => a.kind === b.kind && a.source === b.source

export function CreateSession() {
  const navigate = useNavigate()
  const [name, setName] = useState("")
  const [suggested, setSuggested] = useState(petname) // the placeholder; used when the field is left empty
  const [sessions, setSessions] = useState<PolicySession[]>([])
  // undefined while asking; null where the deployment has no policies.
  const [presets, setPresets] = useState<Preset[] | null | undefined>(undefined)
  const [choice, setChoice] = useState("")
  const [copyFrom, setCopyFrom] = useState("")
  const [custom, setCustom] = useState<PolicySource | null>(null)
  const [customWarnings, setCustomWarnings] = useState(0) // of the policy the panel handed over
  const [managedUrl, setManagedUrl] = useState("")
  const [panelOpen, setPanelOpen] = useState(false)
  const [panelDraft, setPanelDraft] = useState<PolicySource>(NEW_DRAFT)
  const [panelError, setPanelError] = useState("")
  const [panelBusy, setPanelBusy] = useState(false)
  const [confirmPanel, setConfirmPanel] = useState(false) // creating with unapplied changes in the panel
  const [fieldErrors, setFieldErrors] = useState<{ copy?: string; custom?: string; managedUrl?: string }>({})
  const [formError, setFormError] = useState("")
  const [refused, setRefused] = useState<Problems | null>(null) // the 422 of a create
  const [busy, setBusy] = useState(false)
  const gate = useCreateGate() // billing: why a session cannot be created, and what one costs

  useEffect(() => {
    let cancelled = false
    ifAvailable(policyApi.presets())
      .then(list => {
        if (cancelled) return
        const usable = list && list.length > 0 ? list : null
        setPresets(usable)
        if (usable) setChoice(presetChoice(usable[0].id)) // the unrestricted one: Create works untouched
      })
      .catch(e => {
        if (!cancelled && !signedOutHandled(e)) setPresets(null)
      })
    // The sessions already there: names the placeholder should avoid, and
    // policies that can be copied. The form works without them.
    api
      .listSessions()
      .then(list => {
        if (cancelled) return
        setSessions(list)
        const taken = new Set(list.map(s => s.name))
        setSuggested(current => {
          let pet = current
          for (let i = 0; i < 5 && taken.has(pet); i++) pet = petname()
          return pet
        })
      })
      .catch(signedOutHandled)
    return () => {
      cancelled = true
    }
  }, [])

  const copyable = sessions.filter(s => s.policy && s.policy.state !== "unsupported")
  const defaultChoice = presets ? presetChoice(presets[0].id) : ""
  const panelBase = custom ?? NEW_DRAFT
  const panelDirty = panelOpen && !same(panelDraft, panelBase)
  const dirty =
    name.trim() !== "" || choice !== defaultChoice || custom !== null || panelDirty || managedUrl !== "" || copyFrom !== ""
  const unsaved = useUnsavedChanges(dirty)

  function openPanel() {
    setPanelDraft(panelBase)
    setPanelError("")
    setPanelOpen(true)
  }

  function cancelPanel() {
    setPanelDraft(panelBase)
    setPanelError("")
    setPanelOpen(false)
  }

  // "Use this policy": the panel checks its own work before handing it to the form.
  async function usePolicy() {
    if (panelBusy) return
    setPanelBusy(true)
    setPanelError("")
    try {
      const verdict = await policyApi.validate(panelDraft)
      if (!verdict.ok) return setPanelError("Fix the errors in the policy before using it.")
      setCustom(panelDraft)
      setCustomWarnings(verdict.warnings.length)
      setChoice(CUSTOM)
      setFieldErrors(f => ({ ...f, custom: undefined }))
      setRefused(null)
      setPanelOpen(false)
    } catch (e) {
      if (!signedOutHandled(e)) setPanelError(`Couldn't check the policy: ${e instanceof Error ? e.message : e}`)
    } finally {
      setPanelBusy(false)
    }
  }

  async function create(withUnappliedPanel = false) {
    if (busy || gate.disabled) return
    setFormError("")
    gate.clear()
    setRefused(null)
    const errors: typeof fieldErrors = {}
    if (choice === COPY && !copyFrom) errors.copy = "Choose the session to copy the policy from."
    if (choice === CUSTOM && !custom) errors.custom = "Write the policy first: choose Edit policy."
    if (choice === IAC) errors.managedUrl = managedUrlError(managedUrl) || undefined
    setFieldErrors(errors)
    if (Object.values(errors).some(Boolean)) return
    if (panelDirty && !withUnappliedPanel) return setConfirmPanel(true)

    // Left empty, the session gets the name shown as the placeholder.
    const sessionName = name.trim() || suggested
    setBusy(true)
    try {
      let session: PolicySession
      if (presets) {
        let copied: PolicySource | undefined
        if (choice === COPY) {
          const from = await policyApi.getPolicy(copyFrom)
          if (!from.kind || !from.source) throw new ApiError(409, "That session has no policy to copy.")
          copied = { kind: from.kind, source: from.source }
        }
        const policy = policyForChoice({ choice, presets, copied, custom: custom ?? undefined, managedUrl })
        session = await policyApi.createSession({ name: sessionName, policy })
      } else {
        // No policies on this deployment: the request it has always been.
        session = await api.createSession(sessionName)
      }
      unsaved.markSaved()
      const flash: Flash = { type: "success", content: `Session ${session.name} created` }
      navigate(`/sessions/${session.id}`, { state: { flash } })
    } catch (e) {
      if (signedOutHandled(e) || gate.refused(e)) return
      if (e instanceof PolicyApiError && e.status === 422) setRefused({ errors: e.errors, warnings: e.warnings })
      setFormError(String(e instanceof Error ? e.message : e))
    } finally {
      setBusy(false)
    }
  }

  const copyOptions = copyable.map(s => ({
    value: s.id,
    label: s.name,
    description: s.policy?.kind ? `${KIND_LABEL[s.policy.kind]} policy` : undefined,
  }))

  const form = (
    <form
      onSubmit={e => {
        e.preventDefault()
        void create()
      }}
    >
      <Form
        header={<Header variant="h1">Create session</Header>}
        errorText={formError}
        actions={
          <SpaceBetween direction="horizontal" size="xs">
            <Button variant="link" formAction="none" onClick={() => navigate("/")}>
              Cancel
            </Button>
            <Button variant="primary" loading={busy} disabled={gate.disabled} formAction="submit">
              Create session
            </Button>
          </SpaceBetween>
        }
      >
        <SpaceBetween size="l">
          {gate.alert}
          <Container header={<Header variant="h2">Session</Header>}>
            <FormField
              label={
                <>
                  Name - <i>optional</i>
                </>
              }
              description={`Left empty, the session is called ${suggested}.`}
            >
              <Input value={name} placeholder={suggested} onChange={e => setName(e.detail.value)} autoFocus />
            </FormField>
          </Container>

          {presets && (
            <Container
              header={
                <Header
                  variant="h2"
                  description="What an agent connected over MCP may do in the browser, on the desktop and in the shell. It does not restrict you at the screen. You can change it later."
                >
                  Policy
                </Header>
              }
            >
              <SpaceBetween size="m">
                <RadioGroup
                  ariaLabel="Policy"
                  value={choice}
                  onChange={e => setChoice(e.detail.value)}
                  items={[
                    ...presets.map(p => ({ value: presetChoice(p.id), label: p.title, description: p.description })),
                    {
                      value: COPY,
                      label: "Copy from a session",
                      description: "The new session gets its own copy; the two are changed separately.",
                      disabled: copyable.length === 0,
                    },
                    {
                      value: CUSTOM,
                      label: "Write a policy",
                      description: "In Rego, in the editor panel, from a template or one of the policies above.",
                    },
                    {
                      value: IAC,
                      label: "Managed as code",
                      description: "Terraform, OpenTofu or the API. The policy can't be edited here while it is.",
                    },
                  ]}
                />

                {choice === COPY && (
                  <FormField label="Session to copy from" errorText={fieldErrors.copy}>
                    <Select
                      selectedOption={copyOptions.find(o => o.value === copyFrom) ?? null}
                      options={copyOptions}
                      placeholder="Choose a session"
                      onChange={e => setCopyFrom(e.detail.selectedOption.value ?? "")}
                    />
                  </FormField>
                )}

                {choice === CUSTOM && (
                  <FormField label="Policy" errorText={fieldErrors.custom}>
                    <SpaceBetween direction="horizontal" size="s" alignItems="center">
                      <Button formAction="none" onClick={openPanel}>
                        Edit policy
                      </Button>
                      <span data-testid="custom-summary">
                        {custom
                          ? `${KIND_LABEL[custom.kind]} · ${lineCount(custom.source)} lines · valid${customWarnings ? `, ${customWarnings} warning${customWarnings === 1 ? "" : "s"}` : ""}`
                          : "No policy written yet"}
                      </span>
                    </SpaceBetween>
                  </FormField>
                )}

                {choice === IAC && (
                  <FormField
                    label="Link to where it is managed"
                    description="Shown on the session's Policy tab, so the next person knows where to change it."
                    constraintText="Starts with https://. The session has no restrictions until the policy arrives by API token."
                    errorText={fieldErrors.managedUrl}
                  >
                    <Input
                      value={managedUrl}
                      type="url"
                      placeholder="https://github.com/me/infra/blob/main/computeruse/main.tf"
                      onChange={e => setManagedUrl(e.detail.value)}
                    />
                  </FormField>
                )}

                {refused && (
                  <Alert type="error" header="The policy does not validate. No session was created.">
                    <ul className="wf-problems">
                      {refused.errors.map((d, i) => (
                        <li key={i}>{problemLine(d)}</li>
                      ))}
                    </ul>
                  </Alert>
                )}
              </SpaceBetween>
            </Container>
          )}
          {gate.note}
        </SpaceBetween>
      </Form>
    </form>
  )

  const modals = (
    <>
      {unsaved.modal}
      {confirmPanel && (
        <Modal
          visible
          onDismiss={() => setConfirmPanel(false)}
          header="Create without the policy in the editor"
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="xs">
                <Button variant="link" onClick={() => setConfirmPanel(false)}>
                  Cancel
                </Button>
                <Button
                  variant="primary"
                  onClick={() => {
                    setConfirmPanel(false)
                    void create(true)
                  }}
                >
                  Create session
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          The policy in the editor panel has changes that aren&apos;t in use: they are applied with Use this policy. If you
          create the session now, those changes won&apos;t be saved.
        </Modal>
      )}
    </>
  )

  if (presets === undefined) {
    return <Shell breadcrumbs={[{ text: "Create session", href: "/sessions/create" }]}>Loading…</Shell>
  }

  const crumbs = [{ text: "Create session", href: "/sessions/create" }]

  // No policies on this deployment: the form alone, with nothing to write in a panel.
  if (!presets) {
    return (
      <Shell breadcrumbs={crumbs}>
        {form}
        {modals}
      </Shell>
    )
  }

  return (
    <PanelShell
      breadcrumbs={crumbs}
      open={panelOpen}
      onToggle={open => (open ? openPanel() : setPanelOpen(false))}
      panel={
        <SplitPanel
          header="Write a policy"
          hidePreferencesButton
          closeBehavior="hide"
          i18nStrings={{
            closeButtonAriaLabel: "Close the policy editor",
            openButtonAriaLabel: "Open the policy editor",
            resizeHandleAriaLabel: "Resize the policy editor",
          }}
        >
          <SpaceBetween size="m">
            <PolicyWorkbench
              draft={panelDraft}
              onChange={d => {
                setPanelDraft(d)
                setPanelError("")
              }}
              presets={presets}
              editorHeight={240}
            />
            {panelError ? <Alert type="error">{panelError}</Alert> : null}
            <Box float="right">
              <SpaceBetween direction="horizontal" size="xs">
                <Button variant="link" onClick={cancelPanel}>
                  Cancel
                </Button>
                <Button variant="primary" loading={panelBusy} onClick={usePolicy}>
                  Use this policy
                </Button>
              </SpaceBetween>
            </Box>
          </SpaceBetween>
        </SplitPanel>
      }
    >
      {form}
      {modals}
    </PanelShell>
  )
}

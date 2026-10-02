// Single page create for an API token. The secret is in the answer to the
// create request and nowhere else, so the page that made it shows it, once.
import Alert from "@cloudscape-design/components/alert"
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import Checkbox from "@cloudscape-design/components/checkbox"
import Container from "@cloudscape-design/components/container"
import Form from "@cloudscape-design/components/form"
import FormField from "@cloudscape-design/components/form-field"
import Header from "@cloudscape-design/components/header"
import Input from "@cloudscape-design/components/input"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { useState } from "react"
import { useNavigate } from "react-router-dom"
import { signedOutHandled } from "../auth/signedOut"
import type { CreatedToken, NewToken, Scope } from "../policyApi"
import { SCOPES, policyApi } from "../policyApi"
import { Shell } from "../shell"
import { useUnsavedChanges } from "../useUnsavedChanges"
import { TOKENS_CRUMB } from "./Tokens"

const SCOPE_TEXT: Record<Scope, string> = {
  "sessions:read": "List and read your sessions",
  "sessions:write": "Create, rename, stop, resume and delete your sessions",
  "policies:read": "Read the policies of your sessions",
  "policies:write": "Write the policies of your sessions, and say who manages them",
}

const DEFAULT_DAYS = "90"
const MAX_DAYS = 365

export interface TokenForm {
  name: string
  scopes: Scope[]
  days: string
}

export type TokenFormErrors = Partial<Record<"name" | "scopes" | "days", string>>

// The create request for the form, or what is wrong with it.
export function tokenRequest({ name, scopes, days }: TokenForm): { body?: NewToken; errors: TokenFormErrors } {
  const errors: TokenFormErrors = {}
  const trimmed = name.trim()
  if (!trimmed) errors.name = "Enter a name."
  else if (trimmed.length > 64) errors.name = "The name can have at most 64 characters."
  if (scopes.length === 0) errors.scopes = "Choose at least one scope."
  const n = Number(days)
  if (!/^\d+$/.test(days.trim()) || n < 1 || n > MAX_DAYS) errors.days = `Enter a number of days from 1 to ${MAX_DAYS}.`
  if (Object.keys(errors).length > 0) return { errors }
  return { body: { name: trimmed, scopes: SCOPES.filter(s => scopes.includes(s)), expires_in_days: n }, errors }
}

export function CreateToken() {
  const navigate = useNavigate()
  const [name, setName] = useState("")
  const [scopes, setScopes] = useState<Scope[]>([])
  const [days, setDays] = useState(DEFAULT_DAYS)
  const [errors, setErrors] = useState<TokenFormErrors>({})
  const [formError, setFormError] = useState("")
  const [busy, setBusy] = useState(false)
  const [created, setCreated] = useState<CreatedToken | null>(null)
  const [copied, setCopied] = useState<"copied" | "failed" | null>(null)
  // The form's own changes, and then a secret that leaving loses for good.
  const unsaved = useUnsavedChanges(created ? copied !== "copied" : name.trim() !== "" || scopes.length > 0 || days !== DEFAULT_DAYS)

  async function create() {
    if (busy) return
    setFormError("")
    const { body, errors: found } = tokenRequest({ name, scopes, days })
    setErrors(found)
    if (!body) return
    setBusy(true)
    try {
      setCreated(await policyApi.createToken(body))
    } catch (e) {
      if (!signedOutHandled(e)) setFormError(String(e instanceof Error ? e.message : e))
    } finally {
      setBusy(false)
    }
  }

  async function copy(secret: string) {
    try {
      await navigator.clipboard.writeText(secret)
      setCopied("copied")
    } catch {
      setCopied("failed")
    }
  }

  const crumbs = [TOKENS_CRUMB, { text: "Create token", href: "/tokens/create" }]

  if (created) {
    return (
      <Shell breadcrumbs={crumbs}>
        <SpaceBetween size="l">
          <Header variant="h1">Token {created.name} created</Header>
          <Alert type="warning" header="Copy the token now. It is not shown again.">
            Only a hash of it is kept. If you lose it, revoke it and create another.
          </Alert>
          <Container header={<Header variant="h2">Token</Header>}>
            <SpaceBetween size="m">
              <SpaceBetween direction="horizontal" size="xs" alignItems="center">
                <span className="wf-mono" data-testid="token-secret">
                  {created.token}
                </span>
                <Button onClick={() => copy(created.token)}>
                  {copied === "copied" ? "Copied" : copied === "failed" ? "Couldn't copy" : "Copy"}
                </Button>
              </SpaceBetween>
              <Box color="text-body-secondary">
                Scopes: {created.scopes.join(", ")}. Expires {new Date(created.expires).toLocaleDateString()}.
              </Box>
            </SpaceBetween>
          </Container>
          <Alert type="info" header="Keep it away from the agent">
            A policy limits what an agent connected over MCP may do. A token with policies:write can rewrite that
            policy, so a token pasted into a web page, into the session&apos;s /data/memory or into an agent&apos;s
            prompt hands the agent the policy. Keep it where your Terraform or script reads its secrets, and nowhere a
            session can see.
          </Alert>
          <Box float="right">
            <Button variant="primary" onClick={() => navigate("/tokens")}>
              Done
            </Button>
          </Box>
        </SpaceBetween>
        {unsaved.modal}
      </Shell>
    )
  }

  return (
    <Shell breadcrumbs={crumbs}>
      <form
        onSubmit={e => {
          e.preventDefault()
          void create()
        }}
      >
        <Form
          header={<Header variant="h1">Create token</Header>}
          errorText={formError}
          actions={
            <SpaceBetween direction="horizontal" size="xs">
              <Button variant="link" formAction="none" onClick={() => navigate("/tokens")}>
                Cancel
              </Button>
              <Button variant="primary" loading={busy} formAction="submit">
                Create token
              </Button>
            </SpaceBetween>
          }
        >
          <Container header={<Header variant="h2">Token</Header>}>
            <SpaceBetween size="l">
              <FormField label="Name" description="What it is for, so you know which one to revoke." errorText={errors.name}>
                <Input value={name} placeholder="terraform-ci" onChange={e => setName(e.detail.value)} autoFocus />
              </FormField>
              <FormField
                label="Scopes"
                description="What the token may do, on your own sessions only. It never has admin rights."
                errorText={errors.scopes}
              >
                <SpaceBetween size="xs">
                  {SCOPES.map(scope => (
                    <Checkbox
                      key={scope}
                      checked={scopes.includes(scope)}
                      description={SCOPE_TEXT[scope]}
                      onChange={e => setScopes(e.detail.checked ? [...scopes, scope] : scopes.filter(s => s !== scope))}
                    >
                      <span className="wf-mono">{scope}</span>
                    </Checkbox>
                  ))}
                </SpaceBetween>
              </FormField>
              <FormField label="Expires after" constraintText={`Days, from 1 to ${MAX_DAYS}.`} errorText={errors.days}>
                <Input value={days} type="number" inputMode="numeric" onChange={e => setDays(e.detail.value)} />
              </FormField>
            </SpaceBetween>
          </Container>
        </Form>
      </form>
      {unsaved.modal}
    </Shell>
  )
}

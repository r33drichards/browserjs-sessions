// API tokens: what Terraform and scripts sign in with. A view of the caller's
// own tokens; the secret of one is shown once, on the create page.
import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import Header from "@cloudscape-design/components/header"
import Modal from "@cloudscape-design/components/modal"
import SpaceBetween from "@cloudscape-design/components/space-between"
import Table from "@cloudscape-design/components/table"
import { useCallback, useState } from "react"
import { useNavigate } from "react-router-dom"
import { ApiError } from "../api"
import { signedOutHandled } from "../auth/signedOut"
import type { Token } from "../policyApi"
import { policyApi } from "../policyApi"
import { Shell } from "../shell"
import { usePolling } from "../usePolling"

export const TOKENS_CRUMB = { text: "API tokens", href: "/tokens" }

// Last use is recorded to the hour, so it is shown to the hour.
const toTheHour = (when: string) =>
  new Date(when).toLocaleString(undefined, { year: "numeric", month: "short", day: "numeric", hour: "numeric" })

export function Tokens() {
  const navigate = useNavigate()
  const [tokens, setTokens] = useState<Token[] | null>(null)
  const [unavailable, setUnavailable] = useState(false) // the deployment has no tokens
  const [error, setError] = useState("")
  const [revoking, setRevoking] = useState<Token | null>(null)
  const [busy, setBusy] = useState(false)

  const load = useCallback(() => {
    policyApi
      .listTokens()
      .then(list => {
        setTokens(list)
        setError("")
      })
      .catch(e => {
        if (signedOutHandled(e)) return
        if (e instanceof ApiError && (e.status === 404 || e.status === 405)) setUnavailable(true)
        else setError(String(e instanceof Error ? e.message : e))
      })
  }, [])

  usePolling(load, !unavailable, 15000)

  async function revoke(token: Token) {
    setBusy(true)
    try {
      await policyApi.revokeToken(token.id)
      setRevoking(null)
    } catch (e) {
      if (!signedOutHandled(e)) setError(String(e instanceof Error ? e.message : e))
    } finally {
      setBusy(false)
      load()
    }
  }

  if (unavailable) {
    return (
      <Shell breadcrumbs={[TOKENS_CRUMB]}>
        <Box padding="l">API tokens aren&apos;t available on this deployment.</Box>
      </Shell>
    )
  }

  return (
    <Shell breadcrumbs={[TOKENS_CRUMB]}>
      <Table
        loading={tokens === null && !error}
        loadingText="Loading tokens"
        items={tokens ?? []}
        trackBy="id"
        header={
          <Header
            counter={tokens ? `(${tokens.length})` : undefined}
            description="A token acts as you, on your sessions and their policies, from Terraform, OpenTofu or a script. It can't sign in here, create tokens, or act as an admin."
            actions={
              <Button variant="primary" onClick={() => navigate("/tokens/create")}>
                Create token
              </Button>
            }
          >
            API tokens
          </Header>
        }
        columnDefinitions={[
          { id: "name", header: "Name", cell: t => t.name },
          { id: "scopes", header: "Scopes", cell: t => <span className="wf-mono">{t.scopes.join(" ")}</span> },
          { id: "created", header: "Created", cell: t => new Date(t.created).toLocaleDateString() },
          {
            id: "expires",
            header: "Expires",
            cell: t => (new Date(t.expires).getTime() < Date.now() ? "expired" : new Date(t.expires).toLocaleDateString()),
          },
          { id: "last_used", header: "Last used", cell: t => (t.last_used ? toTheHour(t.last_used) : "never") },
          {
            id: "actions",
            header: "",
            cell: t => (
              <Button onClick={() => setRevoking(t)} ariaLabel={`Revoke ${t.name}`}>
                Revoke
              </Button>
            ),
          },
        ]}
        empty={
          <Box textAlign="center" padding="l">
            {error ? `Couldn't load tokens: ${error}` : "No tokens. Create one to manage sessions and policies as code."}
          </Box>
        }
      />
      {error && tokens && tokens.length > 0 && <Box padding={{ top: "s" }}>⚠ {error}</Box>}

      {revoking !== null && (
        <Modal
          visible
          onDismiss={() => setRevoking(null)}
          header="Revoke token"
          footer={
            <Box float="right">
              <SpaceBetween direction="horizontal" size="xs">
                <Button variant="link" onClick={() => setRevoking(null)}>
                  Cancel
                </Button>
                <Button variant="primary" loading={busy} onClick={() => revoking && revoke(revoking)}>
                  Revoke token
                </Button>
              </SpaceBetween>
            </Box>
          }
        >
          Whatever uses {revoking?.name} stops working at once. This can&apos;t be undone.
        </Modal>
      )}
    </Shell>
  )
}

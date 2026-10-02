import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import Header from "@cloudscape-design/components/header"
import SpaceBetween from "@cloudscape-design/components/space-between"
import Table from "@cloudscape-design/components/table"
import Toggle from "@cloudscape-design/components/toggle"
import { useCallback, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import type { Session } from "../api"
import { useMe } from "../auth/MeProvider"
import { signedOutHandled } from "../auth/signedOut"
import { useBilling } from "../billing/BillingProvider"
import { SessionState } from "../billing/SessionBilling"
import { WAKE_BLOCK_LABEL, wakeBlock } from "../billingApi"
import type { PolicySession } from "../policyApi"
import { isManagedAsCode, policySummaryLine } from "../policyApi"
import { Shell, api } from "../shell"
import { usePolling } from "../usePolling"

export function SessionsList() {
  const navigate = useNavigate()
  const me = useMe()
  const { billing } = useBilling()
  const [sessions, setSessions] = useState<PolicySession[] | null>(null)
  const [error, setError] = useState("") // last poll failure; cleared by the next good poll
  const [actionError, setActionError] = useState("") // last failed action; polling leaves it alone
  const [showAll, setShowAll] = useState(false)
  const [copied, setCopied] = useState<{ id: string; ok: boolean } | null>(null) // the row whose button just answered

  const load = useCallback(() => {
    api
      .listSessions(showAll)
      .then(list => {
        setSessions([...list].sort((a, b) => b.created.localeCompare(a.created)))
        setError("")
      })
      .catch(e => {
        if (!signedOutHandled(e)) setError(String(e.message ?? e))
      })
  }, [showAll])

  usePolling(load)

  // The address an MCP client connects to; the same one the session's page shows.
  async function copyMcpUrl(s: Session) {
    let ok = true
    try {
      await navigator.clipboard.writeText(s.mcp_url)
    } catch {
      ok = false
    }
    setCopied({ id: s.id, ok })
    setTimeout(() => setCopied(c => (c?.id === s.id ? null : c)), 1500)
  }

  async function act(fn: () => Promise<unknown>) {
    setActionError("")
    try {
      await fn()
    } catch (e) {
      if (signedOutHandled(e)) return
      setActionError(String(e instanceof Error ? e.message : e))
    }
    load()
  }

  return (
    <Shell>
      <Table
        loading={sessions === null}
        loadingText="Loading sessions"
        items={sessions ?? []}
        trackBy="id"
        header={
          <Header
            counter={sessions ? `(${sessions.length})` : undefined}
            actions={
              <SpaceBetween direction="horizontal" size="s" alignItems="center">
                {me.admin && (
                  <Toggle checked={showAll} onChange={e => setShowAll(e.detail.checked)}>
                    everyone&apos;s
                  </Toggle>
                )}
                <Button variant="primary" onClick={() => navigate("/sessions/create")}>
                  Create session
                </Button>
              </SpaceBetween>
            }
          >
            Sessions
          </Header>
        }
        columnDefinitions={[
          { id: "name", header: "Name", cell: s => <Link to={`/sessions/${s.id}`}>{s.name}</Link> },
          { id: "state", header: "State", cell: s => <SessionState session={s} /> },
          ...(showAll ? [{ id: "owner", header: "Owner", cell: (s: Session) => <span className="wf-mono">{s.owner}</span> }] : []),
          // Only where the backend has policies: without them no session carries one.
          ...(sessions?.some(s => s.policy)
            ? [
                {
                  id: "policy",
                  header: "Policy",
                  cell: (s: PolicySession) =>
                    s.policy && (
                      <>
                        {policySummaryLine(s.policy)}
                        {isManagedAsCode(s.policy) && (
                          <>
                            {" "}
                            <span className="wf-tag">as code</span>
                          </>
                        )}
                      </>
                    ),
                },
              ]
            : []),
          { id: "created", header: "Created", cell: s => new Date(s.created).toLocaleString() },
          {
            id: "actions",
            header: "",
            cell: s => (
              <SpaceBetween direction="horizontal" size="xs">
                <Button onClick={() => copyMcpUrl(s)} ariaLabel={`Copy the MCP URL of ${s.name}`}>
                  {copied?.id === s.id ? (copied.ok ? "Copied" : "Couldn't copy") : "Copy MCP URL"}
                </Button>
                {s.state === "running" || s.state === "starting" ? (
                  <Button onClick={() => act(() => api.setRunning(s.id, false))}>Stop</Button>
                ) : (
                  // A session billing keeps asleep says why instead of resuming.
                  <Button
                    disabled={!!wakeBlock(s, billing)}
                    disabledReason={WAKE_BLOCK_LABEL[wakeBlock(s, billing) ?? "credit"]}
                    onClick={() => act(() => api.setRunning(s.id, true))}
                  >
                    Resume
                  </Button>
                )}
                <Button
                  onClick={() => {
                    if (window.confirm(`Delete "${s.name}" and its disk? This cannot be undone.`))
                      act(() => api.deleteSession(s.id))
                  }}
                >
                  Delete
                </Button>
              </SpaceBetween>
            ),
          },
        ]}
        empty={
          <Box textAlign="center" padding="l">
            {error ? `Couldn't load sessions: ${error}` : "No sessions yet. Create one to get a browser."}
          </Box>
        }
      />
      {actionError && <Box padding={{ top: "s" }}>⚠ {actionError}</Box>}
      {error && sessions && sessions.length > 0 && <Box padding={{ top: "s" }}>⚠ {error}</Box>}
    </Shell>
  )
}

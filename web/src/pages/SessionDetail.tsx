import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import Header from "@cloudscape-design/components/header"
import Input from "@cloudscape-design/components/input"
import SpaceBetween from "@cloudscape-design/components/space-between"
import Tabs from "@cloudscape-design/components/tabs"
import { Suspense, lazy, useCallback, useState } from "react"
import { Link, useNavigate, useSearchParams } from "react-router-dom"
import { ApiError, isSessionId } from "../api"
import { useMe } from "../auth/MeProvider"
import { signedOutHandled } from "../auth/signedOut"
import { BlockedWake, DrainingNote, SessionState, useWakeBlock } from "../billing/SessionBilling"
import { LifecycleActions, stateSentence } from "../components/SessionLifecycle"
import { VncPane } from "../components/VncPane"
import type { PolicySession as Session } from "../policyApi"
import { isManagedAsCode, policySummaryLine } from "../policyApi"
import { Shell, api } from "../shell"
import { HIDDEN_POLL_MS, usePolling } from "../usePolling"

// Only a deployment with policies shows the tab, so only it loads the code.
const PolicyTab = lazy(() => import("../components/PolicyTab").then(m => ({ default: m.PolicyTab })))

// Mounted with key={id}, so every piece of state below starts fresh per session.
export function SessionDetail({ id }: { id: string }) {
  const navigate = useNavigate()
  const me = useMe()
  const [session, setSession] = useState<Session | null>(null)
  // An id the backend could never have issued is "missing" without asking it.
  const [missing, setMissing] = useState(() => !isSessionId(id))
  const [error, setError] = useState("") // last poll failure; cleared by the next good poll
  const [actionError, setActionError] = useState("") // last failed action; polling leaves it alone
  const [name, setName] = useState<string | null>(null) // non-null while editing
  const [deleting, setDeleting] = useState(false)
  const [viewerControls, setViewerControls] = useState<HTMLElement | null>(null)
  const [copied, setCopied] = useState<"copied" | "failed" | null>(null)
  // The open tab is in the address, so the policy's edit page can come back to it.
  const [params, setParams] = useSearchParams()

  const load = useCallback(() => {
    api
      .getSession(id)
      .then(s => {
        setSession(s)
        setError("")
      })
      .catch(e => {
        if (signedOutHandled(e)) return
        if (e instanceof ApiError && e.status === 404) setMissing(true)
        else setError(String(e.message))
      })
  }, [id])

  // A 404 is final: stop asking. Behind another tab it still asks, slowly, so
  // a session that became ready meanwhile is not still "starting" on return.
  usePolling(load, !missing, 3000, HIDDEN_POLL_MS)
  const blocked = useWakeBlock(session) // billing keeps it asleep: no credit, or no card

  if (missing) {
    return (
      <Shell>
        <Box padding="l">This session doesn&apos;t exist, or isn&apos;t yours.</Box>
      </Shell>
    )
  }
  if (!session) return <Shell>{error || "Loading…"}</Shell>

  // Runs an action and reports whether it succeeded; a failure stays on screen
  // until the next action.
  async function act(fn: () => Promise<unknown>): Promise<boolean> {
    setActionError("")
    try {
      await fn()
      return true
    } catch (e) {
      if (signedOutHandled(e)) return false
      setActionError(String(e instanceof Error ? e.message : e))
      return false
    }
  }

  async function actAndReload(fn: () => Promise<unknown>) {
    const ok = await act(fn)
    load()
    return ok
  }

  async function remove(target: Session) {
    if (!window.confirm(`Delete "${target.name}" and its disk? This cannot be undone.`)) return
    setDeleting(true)
    if (await act(() => api.deleteSession(target.id))) return navigate("/")
    setDeleting(false)
    load()
  }

  async function rename(target: Session, to: string) {
    if (!to.trim()) return
    if (await actAndReload(() => api.renameSession(target.id, to))) setName(null)
  }

  async function copyMcpUrl(mcpUrl: string) {
    try {
      await navigator.clipboard.writeText(mcpUrl)
      setCopied("copied")
    } catch {
      setCopied("failed")
    }
    setTimeout(() => setCopied(null), 1500)
  }

  const browser = blocked ? (
    <BlockedWake block={blocked} deleteAfter={session.deleteAfter} />
  ) :
    session.state === "running" ? (
      <>
        <DrainingNote session={session} />
      <VncPane sessionId={session.id} controls={viewerControls} />
      </>
    ) : (
      <div className="wf-placeholder">
        <div>
          {session.state === "starting" && <span className="wf-spinner" aria-hidden="true" />}
          <p>{stateSentence(session)}</p>
          {session.message && <p className="wf-mono">{session.message}</p>}
        </div>
      </div>
    )

  return (
    <Shell breadcrumbs={[{ text: session.name, href: `/sessions/${session.id}` }]}>
      <SpaceBetween size="l">
        {/* The class keeps the actions on the title's line (wireframe.css). */}
        <div className="wf-session-head">
        <Header
          variant="h1"
          actions={
            <SpaceBetween direction="horizontal" size="xs">
              {/* The address an MCP client connects to: add it to Claude as a connector. */}
              <Button onClick={() => copyMcpUrl(session.mcp_url)} ariaLabel={`Copy the MCP URL ${session.mcp_url}`}>
                <span title={session.mcp_url}>
                  {copied === "copied" ? "Copied" : copied === "failed" ? "Couldn't copy" : "Copy MCP URL"}
                </span>
              </Button>
              {/* The viewer puts its Full screen button here. */}
              <span ref={setViewerControls} />
              <LifecycleActions session={session} blocked={blocked} run={actAndReload} primary />
              <Button loading={deleting} onClick={() => remove(session)}>
                Delete
              </Button>
            </SpaceBetween>
          }
        >
          {name === null ? (
            <>
              {session.name}{" "}
              <Button variant="inline-link" onClick={() => setName(session.name)}>
                rename
              </Button>
              <span className="wf-title-meta">
                <SessionState session={session} />
                <span>created {new Date(session.created).toLocaleString()}</span>
                {/* Only an admin looking at someone else's session needs telling whose it is. */}
                {session.owner !== me.email && <span className="wf-mono">{session.owner}</span>}
                {/* The title row is the summary that stays in view on every tab. */}
                {session.policy && (
                  <span>
                    policy: <Link to="?tab=policy">{policySummaryLine(session.policy)}</Link>
                    {isManagedAsCode(session.policy) && (
                      <>
                        {" "}
                        <span className="wf-tag">as code</span>
                      </>
                    )}
                  </span>
                )}
              </span>
            </>
          ) : (
            <SpaceBetween direction="horizontal" size="xs">
              <Input value={name} onChange={e => setName(e.detail.value)} autoFocus />
              <Button disabled={!name.trim()} onClick={() => rename(session, name)}>
                Save
              </Button>
              <Button variant="link" onClick={() => setName(null)}>
                Cancel
              </Button>
            </SpaceBetween>
          )}
        </Header>
        </div>

        {actionError ? <Box>⚠ {actionError}</Box> : null}
        {error ? <Box>⚠ {error}</Box> : null}

        {session.policy ? (
          <Tabs
            ariaLabel="Session"
            activeTabId={params.get("tab") === "policy" ? "policy" : "browser"}
            onChange={e => setParams(e.detail.activeTabId === "policy" ? { tab: "policy" } : {}, { replace: true })}
            tabs={[
              { id: "browser", label: "Browser", content: browser },
              {
                id: "policy",
                label: "Policy",
                content: (
                  <Suspense fallback="Loading the policy">
                    <PolicyTab sessionId={session.id} sessionName={session.name} summary={session.policy} />
                  </Suspense>
                ),
              },
            ]}
          />
        ) : (
          // No policies on this deployment: the browser is the whole page.
          browser
        )}
      </SpaceBetween>
    </Shell>
  )
}

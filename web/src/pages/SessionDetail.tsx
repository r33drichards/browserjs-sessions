import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import ColumnLayout from "@cloudscape-design/components/column-layout"
import Container from "@cloudscape-design/components/container"
import Header from "@cloudscape-design/components/header"
import Input from "@cloudscape-design/components/input"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { useCallback, useState } from "react"
import { useNavigate } from "react-router-dom"
import type { Session } from "../api"
import { ApiError, isSessionId } from "../api"
import { signedOutHandled } from "../auth/signedOut"
import { VncPane } from "../components/VncPane"
import { Shell, StateTag, api } from "../shell"
import { usePolling } from "../usePolling"

const PLACEHOLDER: Record<string, string> = {
  starting: "Starting the browser…",
  stopping: "Stopping…",
  asleep: "Asleep. It wakes when you or an agent uses it.",
  stopped: "Stopped.",
  failed: "The session failed to start.",
}

// Mounted with key={id}, so every piece of state below starts fresh per session.
export function SessionDetail({ id }: { id: string }) {
  const navigate = useNavigate()
  const [session, setSession] = useState<Session | null>(null)
  // An id the backend could never have issued is "missing" without asking it.
  const [missing, setMissing] = useState(() => !isSessionId(id))
  const [error, setError] = useState("") // last poll failure; cleared by the next good poll
  const [actionError, setActionError] = useState("") // last failed action; polling leaves it alone
  const [name, setName] = useState<string | null>(null) // non-null while editing
  const [deleting, setDeleting] = useState(false)
  const [viewerControls, setViewerControls] = useState<HTMLElement | null>(null)
  const [copied, setCopied] = useState<"copied" | "failed" | null>(null)

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

  usePolling(load, !missing) // a 404 is final: stop asking

  if (missing) {
    return (
      <Shell>
        <Box padding="l">This session doesn&apos;t exist, or isn&apos;t yours.</Box>
      </Shell>
    )
  }
  if (!session) return <Shell>{error || "Loading…"}</Shell>

  const awake = session.state === "running" || session.state === "starting"

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

  return (
    <Shell>
      <SpaceBetween size="l">
        <Header
          variant="h1"
          actions={
            <SpaceBetween direction="horizontal" size="xs">
              {/* The viewer puts its Full screen button here. */}
              <span ref={setViewerControls} />
              {awake ? (
                <Button onClick={() => actAndReload(() => api.setRunning(session.id, false))}>Stop</Button>
              ) : (
                <Button variant="primary" onClick={() => actAndReload(() => api.setRunning(session.id, true))}>
                  {session.state === "asleep" ? "Wake" : "Resume"}
                </Button>
              )}
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

        {actionError && <Box>⚠ {actionError}</Box>}
        {error && <Box>⚠ {error}</Box>}

        {session.state === "running" ? (
          <VncPane sessionId={session.id} controls={viewerControls} />
        ) : (
          <div className="wf-placeholder">
            <div>
              {session.state === "starting" && <span className="wf-spinner" aria-hidden="true" />}
              <p>{PLACEHOLDER[session.state]}</p>
              {session.message && <p className="wf-mono">{session.message}</p>}
            </div>
          </div>
        )}

        <Container header={<Header variant="h2">Details</Header>}>
          <ColumnLayout columns={3} variant="text-grid">
            <div>
              <Box variant="awsui-key-label">State</Box>
              <StateTag state={session.state} />
            </div>
            <div>
              <Box variant="awsui-key-label">Created</Box>
              {new Date(session.created).toLocaleString()}
            </div>
            <div>
              <Box variant="awsui-key-label">Owner</Box>
              <span className="wf-mono">{session.owner}</span>
            </div>
          </ColumnLayout>
          <Box margin={{ top: "m" }}>
            <Box variant="awsui-key-label">MCP URL — add this to Claude as a connector</Box>
            <SpaceBetween direction="horizontal" size="xs" alignItems="center">
              <span className="wf-mono">{session.mcp_url}</span>
              <Button onClick={() => copyMcpUrl(session.mcp_url)}>
                {copied === "copied" ? "Copied" : copied === "failed" ? "Couldn't copy" : "Copy"}
              </Button>
            </SpaceBetween>
          </Box>
        </Container>
      </SpaceBetween>
    </Shell>
  )
}

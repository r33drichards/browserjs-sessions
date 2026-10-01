import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import ColumnLayout from "@cloudscape-design/components/column-layout"
import Container from "@cloudscape-design/components/container"
import Header from "@cloudscape-design/components/header"
import Input from "@cloudscape-design/components/input"
import SpaceBetween from "@cloudscape-design/components/space-between"
import { useCallback, useEffect, useState } from "react"
import { useNavigate, useParams } from "react-router-dom"
import type { Session } from "../api"
import { ApiError } from "../api"
import { VncPane } from "../components/VncPane"
import { Shell, StateTag, api } from "../shell"

const PLACEHOLDER: Record<string, string> = {
  starting: "Starting the browser…",
  stopping: "Stopping…",
  asleep: "Asleep. It wakes when you or an agent uses it.",
  stopped: "Stopped.",
  failed: "The session failed to start.",
}

export function SessionDetail() {
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const [session, setSession] = useState<Session | null>(null)
  const [missing, setMissing] = useState(false)
  const [error, setError] = useState("")
  const [name, setName] = useState<string | null>(null) // non-null while editing
  const [copied, setCopied] = useState(false)

  const load = useCallback(() => {
    api
      .getSession(id)
      .then(s => {
        setSession(s)
        setError("")
      })
      .catch(e => (e instanceof ApiError && e.status === 404 ? setMissing(true) : setError(String(e.message))))
  }, [id])

  useEffect(() => {
    load()
    const timer = setInterval(load, 3000)
    return () => clearInterval(timer)
  }, [load])

  if (missing) {
    return (
      <Shell>
        <Box padding="l">This session doesn&apos;t exist, or isn&apos;t yours.</Box>
      </Shell>
    )
  }
  if (!session) return <Shell>{error || "Loading…"}</Shell>

  const mcpUrl = `${window.location.origin}/s/${session.id}/mcp`
  const awake = session.state === "running" || session.state === "starting"

  async function act(fn: () => Promise<unknown>) {
    try {
      await fn()
      setError("")
    } catch (e) {
      setError(String((e as Error).message))
    }
    load()
  }

  return (
    <Shell>
      <SpaceBetween size="l">
        <Header
          variant="h1"
          actions={
            <SpaceBetween direction="horizontal" size="xs">
              {awake ? (
                <Button onClick={() => act(() => api.setRunning(session.id, false))}>Stop</Button>
              ) : (
                <Button variant="primary" onClick={() => act(() => api.setRunning(session.id, true))}>
                  {session.state === "asleep" ? "Wake" : "Resume"}
                </Button>
              )}
              <Button
                onClick={async () => {
                  if (!window.confirm(`Delete "${session.name}" and its disk? This cannot be undone.`)) return
                  await api.deleteSession(session.id)
                  navigate("/")
                }}
              >
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
              <Button
                disabled={!name.trim()}
                onClick={() => act(() => api.renameSession(session.id, name)).then(() => setName(null))}
              >
                Save
              </Button>
              <Button variant="link" onClick={() => setName(null)}>
                Cancel
              </Button>
            </SpaceBetween>
          )}
        </Header>

        {error && <Box>⚠ {error}</Box>}

        {session.state === "running" ? (
          <VncPane sessionId={session.id} />
        ) : (
          <div className="wf-placeholder">
            <div>
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
              <span className="wf-mono">{mcpUrl}</span>
              <Button
                onClick={() => {
                  void navigator.clipboard.writeText(mcpUrl)
                  setCopied(true)
                  setTimeout(() => setCopied(false), 1500)
                }}
              >
                {copied ? "Copied" : "Copy"}
              </Button>
            </SpaceBetween>
          </Box>
        </Container>
      </SpaceBetween>
    </Shell>
  )
}

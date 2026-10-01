import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import FormField from "@cloudscape-design/components/form-field"
import Header from "@cloudscape-design/components/header"
import Input from "@cloudscape-design/components/input"
import Modal from "@cloudscape-design/components/modal"
import SpaceBetween from "@cloudscape-design/components/space-between"
import Table from "@cloudscape-design/components/table"
import Toggle from "@cloudscape-design/components/toggle"
import { useCallback, useEffect, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import type { Session } from "../api"
import { isAdmin } from "../auth/keycloak"
import { Shell, StateTag, api } from "../shell"

export function SessionsList() {
  const navigate = useNavigate()
  const [sessions, setSessions] = useState<Session[] | null>(null)
  const [error, setError] = useState("")
  const [showAll, setShowAll] = useState(false)
  const [creating, setCreating] = useState(false)
  const [name, setName] = useState("")
  const [createError, setCreateError] = useState("")
  const [busy, setBusy] = useState(false)

  const load = useCallback(() => {
    api
      .listSessions(showAll)
      .then(list => {
        setSessions(list.sort((a, b) => b.created.localeCompare(a.created)))
        setError("")
      })
      .catch(e => setError(String(e.message ?? e)))
  }, [showAll])

  useEffect(() => {
    load()
    const timer = setInterval(load, 3000)
    return () => clearInterval(timer)
  }, [load])

  async function create() {
    setBusy(true)
    setCreateError("")
    try {
      const session = await api.createSession(name)
      navigate(`/sessions/${session.id}`)
    } catch (e) {
      setCreateError(String((e as Error).message))
    } finally {
      setBusy(false)
    }
  }

  async function act(fn: () => Promise<unknown>) {
    try {
      await fn()
    } catch (e) {
      setError(String((e as Error).message))
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
                {isAdmin() && (
                  <Toggle checked={showAll} onChange={e => setShowAll(e.detail.checked)}>
                    everyone&apos;s
                  </Toggle>
                )}
                <Button variant="primary" onClick={() => setCreating(true)}>
                  New session
                </Button>
              </SpaceBetween>
            }
          >
            Sessions
          </Header>
        }
        columnDefinitions={[
          { id: "name", header: "Name", cell: s => <Link to={`/sessions/${s.id}`}>{s.name}</Link> },
          { id: "state", header: "State", cell: s => <StateTag state={s.state} /> },
          ...(showAll ? [{ id: "owner", header: "Owner", cell: (s: Session) => <span className="wf-mono">{s.owner}</span> }] : []),
          { id: "created", header: "Created", cell: s => new Date(s.created).toLocaleString() },
          {
            id: "actions",
            header: "",
            cell: s => (
              <SpaceBetween direction="horizontal" size="xs">
                {s.state === "running" || s.state === "starting" ? (
                  <Button onClick={() => act(() => api.setRunning(s.id, false))}>Stop</Button>
                ) : (
                  <Button onClick={() => act(() => api.setRunning(s.id, true))}>Resume</Button>
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
      {error && sessions && sessions.length > 0 && <Box padding={{ top: "s" }}>⚠ {error}</Box>}

      <Modal
        visible={creating}
        onDismiss={() => setCreating(false)}
        header="New session"
        footer={
          <Box float="right">
            <SpaceBetween direction="horizontal" size="xs">
              <Button onClick={() => setCreating(false)}>Cancel</Button>
              <Button variant="primary" loading={busy} disabled={!name.trim()} onClick={create}>
                Create
              </Button>
            </SpaceBetween>
          </Box>
        }
      >
        <FormField label="Name" errorText={createError}>
          <Input value={name} onChange={e => setName(e.detail.value)} autoFocus />
        </FormField>
      </Modal>
    </Shell>
  )
}

import Box from "@cloudscape-design/components/box"
import Button from "@cloudscape-design/components/button"
import FormField from "@cloudscape-design/components/form-field"
import Header from "@cloudscape-design/components/header"
import Input from "@cloudscape-design/components/input"
import Modal from "@cloudscape-design/components/modal"
import SpaceBetween from "@cloudscape-design/components/space-between"
import Table from "@cloudscape-design/components/table"
import Toggle from "@cloudscape-design/components/toggle"
import { useCallback, useState } from "react"
import { Link, useNavigate } from "react-router-dom"
import type { Session } from "../api"
import { useMe } from "../auth/MeProvider"
import { signedOutHandled } from "../auth/signedOut"
import { Shell, StateTag, api } from "../shell"
import { usePolling } from "../usePolling"

export function SessionsList() {
  const navigate = useNavigate()
  const me = useMe()
  const [sessions, setSessions] = useState<Session[] | null>(null)
  const [error, setError] = useState("") // last poll failure; cleared by the next good poll
  const [actionError, setActionError] = useState("") // last failed action; polling leaves it alone
  const [showAll, setShowAll] = useState(false)
  const [creating, setCreating] = useState(false)
  const [name, setName] = useState("")
  const [createError, setCreateError] = useState("")
  const [busy, setBusy] = useState(false)

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

  function openCreate() {
    setName("")
    setCreateError("")
    setCreating(true)
  }

  async function create() {
    if (busy) return
    setBusy(true)
    setCreateError("")
    try {
      // An empty name asks the server to make one up.
      const session = await api.createSession(name.trim())
      navigate(`/sessions/${session.id}`)
    } catch (e) {
      if (!signedOutHandled(e)) setCreateError(String((e as Error).message))
    } finally {
      setBusy(false)
    }
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
                <Button variant="primary" onClick={openCreate}>
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
      {actionError && <Box padding={{ top: "s" }}>⚠ {actionError}</Box>}
      {error && sessions && sessions.length > 0 && <Box padding={{ top: "s" }}>⚠ {error}</Box>}

      <Modal
        visible={creating}
        onDismiss={() => setCreating(false)}
        header="New session"
        footer={
          <Box float="right">
            <SpaceBetween direction="horizontal" size="xs">
              <Button onClick={() => setCreating(false)}>Cancel</Button>
              <Button variant="primary" loading={busy} onClick={create}>
                Create
              </Button>
            </SpaceBetween>
          </Box>
        }
      >
        <FormField
          label={
            <>
              Name - <i>optional</i>
            </>
          }
          errorText={createError}
        >
          <Input
            value={name}
            placeholder="Leave empty for a generated name, like brave-otter"
            onChange={e => setName(e.detail.value)}
            onKeyDown={e => {
              if (e.detail.key === "Enter") void create()
            }}
            autoFocus
          />
        </FormField>
      </Modal>
    </Shell>
  )
}

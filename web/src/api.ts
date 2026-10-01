export type SessionState = "starting" | "running" | "stopping" | "asleep" | "stopped" | "failed"

export interface Session {
  id: string
  name: string
  owner: string
  state: SessionState
  message?: string
  created: string
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
  }
}

// The backend's id format. Anything else never reaches the network, so a
// crafted /sessions/:id link cannot steer an authenticated request elsewhere.
const SESSION_ID = /^s-[a-z2-7]{10}$/

export const isSessionId = (id: string) => SESSION_ID.test(id)

type Fetch = typeof fetch

export function createApi(getToken: () => Promise<string | undefined>, fetchImpl: Fetch = fetch) {
  async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers = new Headers()
    const token = await getToken()
    if (token) headers.set("Authorization", `Bearer ${token}`)
    if (body !== undefined) headers.set("Content-Type", "application/json")
    const res = await fetchImpl(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (!res.ok) {
      let message = `${res.status} ${res.statusText}`
      try {
        message = (await res.json()).error ?? message
      } catch {}
      throw new ApiError(res.status, message)
    }
    return (res.status === 204 ? undefined : await res.json()) as T
  }

  // Path of one session; rejects with a 404 before any request for a malformed id.
  async function sessionPath(id: string, suffix = ""): Promise<string> {
    if (!isSessionId(id)) throw new ApiError(404, "session not found")
    return `/api/sessions/${encodeURIComponent(id)}${suffix}`
  }

  return {
    listSessions: (all = false) => call<Session[]>("GET", all ? "/api/sessions?all=1" : "/api/sessions"),
    getSession: async (id: string) => call<Session>("GET", await sessionPath(id)),
    createSession: (name: string) => call<Session>("POST", "/api/sessions", { name }),
    renameSession: async (id: string, name: string) => call<Session>("PATCH", await sessionPath(id), { name }),
    setRunning: async (id: string, running: boolean) =>
      call<Session>("PATCH", await sessionPath(id), { action: running ? "resume" : "stop" }),
    deleteSession: async (id: string) => call<void>("DELETE", await sessionPath(id)),
    vncTicket: async (id: string) =>
      (await call<{ ticket: string }>("POST", await sessionPath(id, "/vnc-ticket"))).ticket,
  }
}

export type Api = ReturnType<typeof createApi>

export type SessionState = "starting" | "running" | "stopping" | "asleep" | "stopped" | "failed"

export interface Session {
  id: string
  name: string
  owner: string // email
  state: SessionState
  message?: string
  created: string
  mcp_url: string
}

export interface Me {
  email: string
  name: string
  admin: boolean
}

export interface VncTicket {
  ticket: string
  url: string // websocket URL on the session's own host, ticket included
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
  }
}

// The identity proxy in front of the app no longer accepts our session cookie.
// Only a full page load can fix that: the proxy then redirects to sign-in.
export class SignedOutError extends Error {
  constructor() {
    super("You've been signed out")
  }
}

// The backend's id format. Anything else never reaches the network, so a
// crafted /sessions/:id link cannot steer an authenticated request elsewhere.
// Ten base32 characters for a session the backend named, five characters for
// one taken from the warm pool (backend/internal/sessions ValidID).
const SESSION_ID = /^s-([a-z2-7]{10}|[a-z0-9]{5})$/

export const isSessionId = (id: string) => SESSION_ID.test(id)

const isJson = (res: Response) => /^application\/([\w.-]+\+)?json\b/i.test(res.headers.get("Content-Type") ?? "")

type Fetch = typeof fetch

export function createApi(fetchImpl: Fetch = fetch) {
  async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers = new Headers({ Accept: "application/json" })
    if (body !== undefined) headers.set("Content-Type", "application/json")
    // The proxy's cookie authenticates the request. An expired proxy session
    // shows up as a 401, as a redirect to the sign-in page (kept opaque by
    // redirect: "manual" rather than followed), or as that page's HTML.
    const res = await fetchImpl(path, {
      method,
      headers,
      credentials: "same-origin",
      redirect: "manual",
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (res.type === "opaqueredirect" || res.status === 401) throw new SignedOutError()
    if (!res.ok) {
      let message = `${res.status} ${res.statusText}`
      try {
        message = (await res.json()).error ?? message
      } catch {}
      throw new ApiError(res.status, message)
    }
    if (res.status === 204) return undefined as T
    if (!isJson(res)) throw new SignedOutError()
    return (await res.json()) as T
  }

  // Path of one session; rejects with a 404 before any request for a malformed id.
  async function sessionPath(id: string, suffix = ""): Promise<string> {
    if (!isSessionId(id)) throw new ApiError(404, "session not found")
    return `/api/sessions/${encodeURIComponent(id)}${suffix}`
  }

  return {
    me: () => call<Me>("GET", "/api/me"),
    listSessions: (all = false) => call<Session[]>("GET", all ? "/api/sessions?all=1" : "/api/sessions"),
    getSession: async (id: string) => call<Session>("GET", await sessionPath(id)),
    createSession: (name: string) => call<Session>("POST", "/api/sessions", { name }),
    renameSession: async (id: string, name: string) => call<Session>("PATCH", await sessionPath(id), { name }),
    setRunning: async (id: string, running: boolean) =>
      call<Session>("PATCH", await sessionPath(id), { action: running ? "resume" : "stop" }),
    deleteSession: async (id: string) => call<void>("DELETE", await sessionPath(id)),
    vncTicket: async (id: string) => call<VncTicket>("POST", await sessionPath(id, "/vnc-ticket")),
  }
}

export type Api = ReturnType<typeof createApi>

export const api = createApi()

export type SessionState = "starting" | "running" | "stopping" | "asleep" | "stopped" | "failed"

export interface Session {
  id: string
  name: string
  owner: string // email
  state: SessionState
  message?: string
  created: string
  mcp_url: string
  // A session that is asleep holds a snapshot of its pod to wake from. Absent
  // (asleep without one, or stopped), it starts fresh, with its disk only.
  stateSaved?: boolean
  // Only where the backend has billing on (docs/contracts/billing/backend-api.yaml).
  stoppedBy?: StoppedBy // why it is asleep or stopped
  draining?: StoppedBy // finishing calls before such a sleep
  deleteAfter?: string // due for deletion at this time
}

export type StoppedBy = "user" | "sleep" | "idle" | "credit" | "payment-method" | "blocked"

export interface Me {
  email: string
  name: string
  admin: boolean
}

export interface VncTicket {
  ticket: string
  url: string // websocket URL on the sessions' host, ticket included
}

// A file in the session's folder: where its browser downloads to, and where
// its file chooser opens.
export interface SessionFile {
  name: string
  size: number // bytes
  modified: string
}

export interface SessionFiles {
  files: SessionFile[]
  max_bytes: number // the largest file that may be sent
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message)
  }
  // The `Error` of the billing contract, when the backend refused for billing.
  code?: string
  billingUrl?: string
  limit?: number
}

// Copies the billing contract's fields of an error answer onto the error.
export function withRefusal<E extends ApiError>(error: E, body: unknown): E {
  const b = (body ?? {}) as { code?: unknown; billingUrl?: unknown; limit?: unknown }
  if (typeof b.code === "string") error.code = b.code
  if (typeof b.billingUrl === "string") error.billingUrl = b.billingUrl
  if (typeof b.limit === "number") error.limit = b.limit
  return error
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
      let detail: unknown
      try {
        detail = await res.json()
        message = (detail as { error?: string }).error ?? message
      } catch {}
      throw withRefusal(new ApiError(res.status, message), detail)
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
    // Snapshots the pod, then suspends: answers once the snapshot is done.
    sleepSession: async (id: string) => call<Session>("POST", await sessionPath(id, "/sleep")),
    // Starts a session that is asleep (from its snapshot) or stopped (fresh).
    wakeSession: async (id: string) => call<Session>("POST", await sessionPath(id, "/wake")),
    deleteSession: async (id: string) => call<void>("DELETE", await sessionPath(id)),
    vncTicket: async (id: string) => call<VncTicket>("POST", await sessionPath(id, "/vnc-ticket")),
    listFiles: async (id: string) => call<SessionFiles>("GET", await sessionPath(id, "/files")),
    deleteFile: async (id: string, name: string) =>
      call<void>("DELETE", await sessionPath(id, `/files/${encodeURIComponent(name)}`)),
    // Puts files of the session's folder on its browser's clipboard, as one
    // selection: Ctrl+V there pastes them.
    copyFiles: async (id: string, names: string[]) => call<void>("POST", await sessionPath(id, "/clipboard"), { files: names }),
  }
}

export type Api = ReturnType<typeof createApi>

export const api = createApi()

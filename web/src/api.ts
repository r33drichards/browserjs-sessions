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

  return {
    listSessions: (all = false) => call<Session[]>("GET", all ? "/api/sessions?all=1" : "/api/sessions"),
    getSession: (id: string) => call<Session>("GET", `/api/sessions/${id}`),
    createSession: (name: string) => call<Session>("POST", "/api/sessions", { name }),
    renameSession: (id: string, name: string) => call<Session>("PATCH", `/api/sessions/${id}`, { name }),
    setRunning: (id: string, running: boolean) =>
      call<Session>("PATCH", `/api/sessions/${id}`, { action: running ? "resume" : "stop" }),
    deleteSession: (id: string) => call<void>("DELETE", `/api/sessions/${id}`),
    vncTicket: async (id: string) =>
      (await call<{ ticket: string }>("POST", `/api/sessions/${id}/vnc-ticket`)).ticket,
  }
}

export type Api = ReturnType<typeof createApi>

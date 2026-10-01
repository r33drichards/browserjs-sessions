import { afterEach, describe, expect, it, vi } from "vitest"
import { ApiError, SignedOutError, createApi, isSessionId } from "./api"

const JSON_HEADERS = { "Content-Type": "application/json" }

function fakeFetch(status: number, body: unknown) {
  return vi.fn(
    async (_input: RequestInfo | URL, _init?: RequestInit) =>
      new Response(body === undefined ? null : JSON.stringify(body), { status, headers: JSON_HEADERS }),
  )
}

const session = { id: "s-aaaaaaaaaa", name: "a", owner: "u@example.com", state: "running", created: "2026-10-01T00:00:00Z", mcp_url: "https://s-aaaaaaaaaa.example.com/mcp" }

afterEach(() => vi.restoreAllMocks())

describe("api", () => {
  it("parses sessions and relies on the proxy cookie, not a token", async () => {
    const fetch = fakeFetch(200, [session])
    const api = createApi(fetch)
    const list = await api.listSessions()
    expect(list[0].id).toBe("s-aaaaaaaaaa")
    expect(list[0].mcp_url).toBe("https://s-aaaaaaaaaa.example.com/mcp")
    const [url, init] = fetch.mock.calls[0] as [string, RequestInit]
    expect(url).toBe("/api/sessions")
    const headers = new Headers(init.headers)
    expect(headers.has("Authorization")).toBe(false)
    expect(headers.get("Accept")).toBe("application/json")
    expect(init.credentials).toBe("same-origin")
    expect(init.redirect).toBe("manual")
  })

  it("sends no Authorization header on writes either", async () => {
    const fetch = fakeFetch(200, session)
    const api = createApi(fetch)
    await api.createSession("a")
    await api.renameSession("s-aaaaaaaaaa", "b")
    await api.vncTicket("s-aaaaaaaaaa")
    for (const [, init] of fetch.mock.calls) expect(new Headers(init?.headers).has("Authorization")).toBe(false)
  })

  it("fetches the signed-in user", async () => {
    const fetch = fakeFetch(200, { email: "u@example.com", name: "U", admin: true })
    await expect(createApi(fetch).me()).resolves.toEqual({ email: "u@example.com", name: "U", admin: true })
    expect(fetch.mock.calls[0][0]).toBe("/api/me")
  })

  it("posts a name to create and a stop action to patch", async () => {
    const fetch = fakeFetch(201, { ...session, id: "s-bbbbbbbbbb", state: "starting" })
    const api = createApi(fetch)
    await api.createSession("b")
    await api.setRunning("s-bbbbbbbbbb", false)
    const [, create] = fetch.mock.calls[0] as [string, RequestInit]
    expect(create.method).toBe("POST")
    expect(create.body).toBe(JSON.stringify({ name: "b" }))
    const [url, patch] = fetch.mock.calls[1] as [string, RequestInit]
    expect(url).toBe("/api/sessions/s-bbbbbbbbbb")
    expect(patch.body).toBe(JSON.stringify({ action: "stop" }))
  })

  it("throws ApiError carrying the server's message", async () => {
    const api = createApi(fakeFetch(409, { error: "session limit reached; delete one first" }))
    await expect(api.createSession("x")).rejects.toMatchObject({
      status: 409,
      message: "session limit reached; delete one first",
    })
    await expect(api.createSession("x")).rejects.toBeInstanceOf(ApiError)
  })

  it("accepts an empty 204 on delete", async () => {
    const fetch = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(null, { status: 204 }))
    await expect(createApi(fetch).deleteSession("s-aaaaaaaaaa")).resolves.toBeUndefined()
  })

  it("rejects a malformed session id without making a request", async () => {
    const fetch = fakeFetch(200, {})
    const api = createApi(fetch)
    const bad = "../../s/s-aaaaaaaaaa/mcp"
    const calls = [
      () => api.getSession(bad),
      () => api.renameSession(bad, "x"),
      () => api.setRunning(bad, true),
      () => api.deleteSession(bad),
      () => api.vncTicket(bad),
    ]
    for (const call of calls) {
      const error = await call().then(
        () => null,
        (e: unknown) => e,
      )
      expect(error).toBeInstanceOf(ApiError)
      expect(error).toMatchObject({ status: 404 })
    }
    expect(fetch).not.toHaveBeenCalled()
  })

  it("returns the vnc ticket and the URL to connect to", async () => {
    const ticket = { ticket: "t", url: "wss://s-abcdefg234.example.com/vnc?ticket=t" }
    const fetch = fakeFetch(200, ticket)
    await expect(createApi(fetch).vncTicket("s-abcdefg234")).resolves.toEqual(ticket)
    const [url, init] = fetch.mock.calls[0] as [string, RequestInit]
    expect(url).toBe("/api/sessions/s-abcdefg234/vnc-ticket")
    expect(init.method).toBe("POST")
  })
})

describe("signed-out detection", () => {
  const rejection = (response: Response) => createApi(vi.fn(async () => response)).listSessions()

  it("treats a 401 as signed out, whatever its body", async () => {
    await expect(rejection(new Response(JSON.stringify({ error: "unauthorized" }), { status: 401, headers: JSON_HEADERS }))).rejects.toBeInstanceOf(SignedOutError)
    await expect(rejection(new Response("<html>", { status: 401, headers: { "Content-Type": "text/html" } }))).rejects.toBeInstanceOf(SignedOutError)
  })

  it("treats an opaque redirect as signed out", async () => {
    // What fetch reports for a 3xx under redirect: "manual"; not constructible directly.
    const response = Object.defineProperties(new Response(null), {
      type: { value: "opaqueredirect" },
      status: { value: 0 },
      ok: { value: false },
    })
    await expect(rejection(response)).rejects.toBeInstanceOf(SignedOutError)
  })

  it("treats a 2xx that is not JSON as signed out", async () => {
    const page = () => new Response("<!doctype html><title>Sign in</title>", { status: 200, headers: { "Content-Type": "text/html; charset=utf-8" } })
    await expect(rejection(page())).rejects.toBeInstanceOf(SignedOutError)
    await expect(rejection(new Response("ok", { status: 200 }))).rejects.toBeInstanceOf(SignedOutError)
  })

  it("keeps other failures as ordinary ApiErrors", async () => {
    for (const status of [403, 404, 500]) {
      const error = await rejection(new Response("<html>", { status, headers: { "Content-Type": "text/html" } })).then(
        () => null,
        (e: unknown) => e,
      )
      expect(error).toBeInstanceOf(ApiError)
      expect(error).not.toBeInstanceOf(SignedOutError)
    }
  })
})

describe("isSessionId", () => {
  it("accepts only s- followed by ten base32 characters", () => {
    expect(isSessionId("s-abcdefg234")).toBe(true)
    for (const bad of ["", "s-1", "s-abcdefg23", "s-abcdefg2345", "s-ABCDEFG234", "s-abcdefg189", "x-abcdefg234", "s-abcdefg234/", "s-abcdefg234\n", "../s-abcdefg234"]) {
      expect(isSessionId(bad), bad).toBe(false)
    }
  })
})

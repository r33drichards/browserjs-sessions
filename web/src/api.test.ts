import { afterEach, describe, expect, it, vi } from "vitest"
import { ApiError, createApi, isSessionId } from "./api"

function fakeFetch(status: number, body: unknown) {
  return vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(body === undefined ? null : JSON.stringify(body), { status }))
}

afterEach(() => vi.restoreAllMocks())

describe("api", () => {
  it("sends the bearer token and parses sessions", async () => {
    const fetch = fakeFetch(200, [{ id: "s-aaaaaaaaaa", name: "a", owner: "u", state: "running", created: "2026-10-01T00:00:00Z" }])
    const api = createApi(async () => "tok", fetch)
    const list = await api.listSessions()
    expect(list[0].id).toBe("s-aaaaaaaaaa")
    const [url, init] = fetch.mock.calls[0] as [string, RequestInit]
    expect(url).toBe("/api/sessions")
    expect(new Headers(init.headers).get("Authorization")).toBe("Bearer tok")
  })

  it("posts a name to create and a stop action to patch", async () => {
    const fetch = fakeFetch(201, { id: "s-bbbbbbbbbb", name: "b", owner: "u", state: "starting", created: "" })
    const api = createApi(async () => "tok", fetch)
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
    const api = createApi(async () => "tok", fakeFetch(409, { error: "session limit reached; delete one first" }))
    await expect(api.createSession("x")).rejects.toMatchObject({
      status: 409,
      message: "session limit reached; delete one first",
    })
    await expect(api.createSession("x")).rejects.toBeInstanceOf(ApiError)
  })

  it("accepts an empty 204 on delete", async () => {
    const api = createApi(async () => "tok", fakeFetch(204, undefined))
    await expect(api.deleteSession("s-aaaaaaaaaa")).resolves.toBeUndefined()
  })

  it("rejects a malformed session id without making a request", async () => {
    const fetch = fakeFetch(200, {})
    const api = createApi(async () => "tok", fetch)
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

  it("requests the vnc ticket for a valid id", async () => {
    const fetch = fakeFetch(200, { ticket: "t" })
    const api = createApi(async () => "tok", fetch)
    await expect(api.vncTicket("s-abcdefg234")).resolves.toBe("t")
    expect(fetch.mock.calls[0][0]).toBe("/api/sessions/s-abcdefg234/vnc-ticket")
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

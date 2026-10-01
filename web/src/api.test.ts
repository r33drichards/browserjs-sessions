import { afterEach, describe, expect, it, vi } from "vitest"
import { ApiError, createApi } from "./api"

function fakeFetch(status: number, body: unknown) {
  return vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(body === undefined ? null : JSON.stringify(body), { status }))
}

afterEach(() => vi.restoreAllMocks())

describe("api", () => {
  it("sends the bearer token and parses sessions", async () => {
    const fetch = fakeFetch(200, [{ id: "s-1", name: "a", owner: "u", state: "running", created: "2026-10-01T00:00:00Z" }])
    const api = createApi(async () => "tok", fetch)
    const list = await api.listSessions()
    expect(list[0].id).toBe("s-1")
    const [url, init] = fetch.mock.calls[0] as [string, RequestInit]
    expect(url).toBe("/api/sessions")
    expect(new Headers(init.headers).get("Authorization")).toBe("Bearer tok")
  })

  it("posts a name to create and a stop action to patch", async () => {
    const fetch = fakeFetch(201, { id: "s-2", name: "b", owner: "u", state: "starting", created: "" })
    const api = createApi(async () => "tok", fetch)
    await api.createSession("b")
    await api.setRunning("s-2", false)
    const [, create] = fetch.mock.calls[0] as [string, RequestInit]
    expect(create.method).toBe("POST")
    expect(create.body).toBe(JSON.stringify({ name: "b" }))
    const [url, patch] = fetch.mock.calls[1] as [string, RequestInit]
    expect(url).toBe("/api/sessions/s-2")
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
    await expect(api.deleteSession("s-1")).resolves.toBeUndefined()
  })
})

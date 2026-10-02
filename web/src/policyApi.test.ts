import { describe, expect, it, vi } from "vitest"
import { presetDescription, presetsFromExamples } from "../mock/backend"
import { ApiError, SignedOutError } from "./api"
import {
  PolicyApiError,
  createPolicyApi,
  ifAvailable,
  isUnrestricted,
  managedUrlError,
  policyStatus,
  policySummaryLine,
  updatedByLabel,
} from "./policyApi"

function fakeFetch(status: number, body: unknown, contentType = "application/json") {
  return vi.fn(
    async (_input: RequestInfo | URL, _init?: RequestInit) =>
      new Response(body === undefined ? null : typeof body === "string" ? body : JSON.stringify(body), {
        status,
        headers: { "Content-Type": contentType },
      }),
  )
}

const ID = "s-aaaaaaaaaa"
const SOURCE = "package browserjs.policy\n"
const policy = { kind: "rego", version: 3, state: "ready", source: SOURCE, management: { mode: "editor" } }
const input = { kind: "rego", source: SOURCE } as const

const examples = import.meta.glob("../../docs/contracts/policy/examples/*", { query: "?raw", import: "default", eager: true }) as Record<string, string>
const presets = presetsFromExamples(examples)

describe("policy api", () => {
  it("reads a policy from the session's policy path, with the proxy cookie and no token", async () => {
    const fetch = fakeFetch(200, policy)
    await expect(createPolicyApi(fetch).getPolicy(ID)).resolves.toMatchObject({ version: 3 })
    const [url, init] = fetch.mock.calls[0] as [string, RequestInit]
    expect(url).toBe(`/api/sessions/${ID}/policy`)
    expect(init.method).toBe("GET")
    expect(init.credentials).toBe("same-origin")
    expect(new Headers(init.headers).has("Authorization")).toBe(false)
  })

  it("never asks for a malformed session id", async () => {
    const fetch = fakeFetch(200, policy)
    const api = createPolicyApi(fetch)
    await expect(api.getPolicy("../../me")).rejects.toMatchObject({ status: 404 })
    await expect(api.putPolicy("create", input)).rejects.toBeInstanceOf(ApiError)
    expect(fetch).not.toHaveBeenCalled()
  })

  it("saves with If-Match set to the quoted version, and tells 200 from 202", async () => {
    const ok = fakeFetch(200, { ...policy, version: 4 })
    const saved = await createPolicyApi(ok).putPolicy(ID, input, 3)
    expect(saved).toMatchObject({ inForce: true, policy: { version: 4 } })
    const [url, init] = ok.mock.calls[0] as [string, RequestInit]
    expect(url).toBe(`/api/sessions/${ID}/policy`)
    expect(init.method).toBe("PUT")
    expect(new Headers(init.headers).get("If-Match")).toBe('"3"')
    expect(init.body).toBe(JSON.stringify(input))

    const accepted = fakeFetch(202, { ...policy, version: 4, state: "loading" })
    await expect(createPolicyApi(accepted).putPolicy(ID, input)).resolves.toMatchObject({ inForce: false })
    expect(new Headers((accepted.mock.calls[0] as [string, RequestInit])[1].headers).has("If-Match")).toBe(false)
  })

  it("resets with DELETE and changes the mode with PUT on /management", async () => {
    const fetch = fakeFetch(200, policy)
    const api = createPolicyApi(fetch)
    await api.resetPolicy(ID)
    await api.setManagement(ID, { mode: "iac", managed_url: "https://example.com/main.tf" })
    const [reset, mode] = fetch.mock.calls as [string, RequestInit][]
    expect([reset[0], reset[1].method]).toEqual([`/api/sessions/${ID}/policy`, "DELETE"])
    expect([mode[0], mode[1].method]).toEqual([`/api/sessions/${ID}/policy/management`, "PUT"])
    expect(mode[1].body).toBe(JSON.stringify({ mode: "iac", managed_url: "https://example.com/main.tf" }))
  })

  it("carries the diagnostics of a 422 and the link of a mode refusal", async () => {
    const errors = [{ row: 4, col: 21, code: "unknown_operation", message: 'unknown operation "clik"' }]
    const invalid = createPolicyApi(fakeFetch(422, { error: "the policy does not validate", errors, warnings: [] }))
    const e422 = await invalid.putPolicy(ID, input).catch(e => e)
    expect(e422).toBeInstanceOf(PolicyApiError)
    expect(e422).toMatchObject({ status: 422, message: "the policy does not validate", errors })

    const refused = createPolicyApi(fakeFetch(409, { error: "this policy is managed externally", managed_url: "https://example.com/main.tf" }))
    await expect(refused.putPolicy(ID, input)).rejects.toMatchObject({
      status: 409,
      managedUrl: "https://example.com/main.tf",
    })
    await expect(createPolicyApi(fakeFetch(412, { error: "changed" })).putPolicy(ID, input, 1)).rejects.toMatchObject({ status: 412 })
  })

  it("posts the source to validate and the source with an input to evaluate", async () => {
    const fetch = fakeFetch(200, { ok: true, errors: [], warnings: [] })
    const api = createPolicyApi(fetch)
    await api.validate({ kind: "rego", source: "package browserjs.policy" })
    await api.evaluate({ kind: "rego", source: "package browserjs.policy" }, { tool: "browser_execute" })
    const [validate, evaluate] = fetch.mock.calls as [string, RequestInit][]
    expect(validate[0]).toBe("/api/policies/validate")
    expect(validate[1].body).toBe(JSON.stringify({ kind: "rego", source: "package browserjs.policy" }))
    expect(evaluate[0]).toBe("/api/policies/evaluate")
    expect(JSON.parse(String(evaluate[1].body))).toEqual({ kind: "rego", source: "package browserjs.policy", input: { tool: "browser_execute" } })
  })

  it("creates a session with its policy, and has no schema to ask for", async () => {
    const fetch = fakeFetch(201, { id: ID })
    await createPolicyApi(fetch).createSession({ name: "a", policy: { ...input, management: { mode: "iac", managed_url: "https://x.example" } } })
    const [url, init] = fetch.mock.calls[0] as [string, RequestInit]
    expect([url, init.method]).toEqual(["/api/sessions", "POST"])
    expect(JSON.parse(String(init.body)).policy.management.mode).toBe("iac")

    expect(createPolicyApi(fetch)).not.toHaveProperty("schema")
  })

  it("lists, creates and revokes tokens", async () => {
    const fetch = fakeFetch(201, { id: "t1", token: "bjs_t1_secret" })
    const api = createPolicyApi(fetch)
    await expect(api.createToken({ name: "ci", scopes: ["policies:write"], expires_in_days: 30 })).resolves.toMatchObject({ token: "bjs_t1_secret" })
    const gone = fakeFetch(204, undefined)
    await expect(createPolicyApi(gone).revokeToken("t/1")).resolves.toBeUndefined()
    expect(gone.mock.calls[0][0]).toBe("/api/tokens/t%2F1")
    expect((gone.mock.calls[0] as [string, RequestInit])[1].method).toBe("DELETE")
  })

  it("treats a signed-out proxy as signed out, not as a missing feature", async () => {
    await expect(createPolicyApi(fakeFetch(401, {})).presets()).rejects.toBeInstanceOf(SignedOutError)
    await expect(createPolicyApi(fakeFetch(200, "<html>sign in</html>", "text/html")).presets()).rejects.toBeInstanceOf(SignedOutError)
    await expect(ifAvailable(createPolicyApi(fakeFetch(401, {})).presets())).rejects.toBeInstanceOf(SignedOutError)
  })

  it("reports a feature the backend does not have as absent", async () => {
    // What Go's mux answers for a route nobody registered.
    const off = createPolicyApi(fakeFetch(404, "404 page not found\n", "text/plain"))
    await expect(ifAvailable(off.presets())).resolves.toBeNull()
    await expect(ifAvailable(off.listTokens())).resolves.toBeNull()
    await expect(ifAvailable(createPolicyApi(fakeFetch(200, [{ id: "unrestricted" }])).presets())).resolves.toHaveLength(1)
  })
})

describe("policy state", () => {
  it("maps state and loaded to the status line", () => {
    expect(policyStatus({ state: "ready", loaded: { replicas: 2, total: 2 } })).toBe("in force (2/2)")
    expect(policyStatus({ state: "ready" })).toBe("in force")
    expect(policyStatus({ state: "loading", loaded: { replicas: 1, total: 2 } })).toBe("loading (1/2)")
    expect(policyStatus({ state: "invalid", hash: "sha256:9f2c" })).toBe("does not compile; the previous policy is still in force")
    expect(policyStatus({ state: "invalid" })).toBe("does not compile")
    expect(policyStatus({ state: "unsupported" })).toBe("not supported")
  })

  it("summarises a policy in one line", () => {
    expect(policySummaryLine({ kind: "rego", version: 3, state: "ready" })).toBe("Rego, v3, in force")
    expect(policySummaryLine({ kind: "rego", version: 7, state: "loading" })).toBe("Rego, v7, loading")
    expect(policySummaryLine({ state: "unsupported" })).toBe("none (created before policies)")
  })

  it("says who saved it", () => {
    expect(updatedByLabel("ui")).toBe("in the UI")
    expect(updatedByLabel("token:ci")).toBe('by token "ci"')
    expect(updatedByLabel(undefined)).toBe("")
  })

  it("recognises the unrestricted policy, whatever its comments and spacing", () => {
    const rego = (source: string) => ({ kind: "rego" as const, source })
    expect(isUnrestricted(rego("package browserjs.policy\n\nimport rego.v1\n\nallow_tool_call := true\n"))).toBe(true)
    expect(isUnrestricted(rego("# all of it\npackage browserjs.policy\nimport rego.v1 # v1\n\tallow_tool_call  :=  true"))).toBe(true)
    expect(isUnrestricted(rego("package browserjs.policy\nimport rego.v1\nallow_tool_call := false\n"))).toBe(false)
    expect(isUnrestricted(rego("package browserjs.policy\nimport rego.v1\nallow_tool_call := true\nx := 1\n"))).toBe(false)
    // Commented out, it allows nothing.
    expect(isUnrestricted(rego("package browserjs.policy\nimport rego.v1\n# allow_tool_call := true\n"))).toBe(false)
    expect(isUnrestricted(rego("package browserjs.policy"))).toBe(false)
    expect(isUnrestricted({ kind: "rego", source: "" })).toBe(false)
    expect(isUnrestricted({ source: "package browserjs.policy\nimport rego.v1\nallow_tool_call := true\n" })).toBe(false)
  })

  it("calls the unrestricted preset unrestricted, and no other preset", () => {
    expect(presets.filter(isUnrestricted).map(p => p.id)).toEqual(["unrestricted"])
  })

  it("requires an https link for a policy managed as code", () => {
    expect(managedUrlError("")).not.toBe("")
    expect(managedUrlError("http://example.com/main.tf")).toMatch(/https/)
    expect(managedUrlError("not a link")).not.toBe("")
    expect(managedUrlError(" https://github.com/me/infra ")).toBe("")
  })
})

describe("presets", () => {
  it("are the contract's Rego examples, the unrestricted one first", () => {
    expect(presets.map(p => p.id)).toEqual(["unrestricted", "browser-only", "form-filling", "no-scripting", "observe-only", "one-site", "read-only-shell"])
    for (const p of presets) {
      expect(p.kind).toBe("rego")
      expect(p.source).toMatch(/^package browserjs\.policy$/m)
      expect(p.cases.length).toBeGreaterThan(0)
    }
    expect(presets.reduce((n, p) => n + p.cases.length, 0)).toBe(222)
  })

  it("take the title from the id and the description from the comment the file begins with", () => {
    expect(presets.map(p => p.title)).toEqual(["Unrestricted", "Browser only", "Form filling", "No scripting", "Observe only", "One site", "Read-only shell"])
    expect(presets[0].description).toBe("No restrictions: every operation in the browser, full control of the desktop, and any shell command.")
    expect(presetDescription("# a\n#   b  c\n#\n# d\npackage x\n# not this\n")).toBe("a b c d")
    for (const p of presets) expect(p.description).not.toBe("")
  })
})

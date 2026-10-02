// The session-policy and API-token endpoints, as docs/contracts/policy/backend-api.yaml
// has them. Kept apart from api.ts: a deployment without the feature never
// answers these, and the pages that use them must then show nothing.
import type { Session } from "./api"
import { ApiError, SignedOutError, isSessionId, withRefusal } from "./api"

// A policy is a Rego module. `kind` stays in the API with this one value.
export type PolicyKind = "rego"
export type PolicyState = "ready" | "loading" | "invalid" | "unsupported"
export type ManagementMode = "editor" | "iac"

export interface Management {
  mode: ManagementMode
  managed_url?: string // required, https, when mode is iac
}

export interface PolicySummary {
  kind?: PolicyKind
  version?: number
  hash?: string
  state: PolicyState
  management?: Management
}

// 1-based positions in the source; absent when the problem has no place.
export interface Diagnostic {
  row?: number
  col?: number
  code: string
  message: string
}

export interface Policy extends PolicySummary {
  source?: string
  rego?: string // the module in force: the last source that compiled
  errors?: Diagnostic[]
  warnings?: Diagnostic[]
  loaded?: { replicas?: number; total?: number }
  updated?: string
  updated_by?: string // "ui" or "token:<token name>"
}

export interface PolicySource {
  kind: PolicyKind
  source: string
}

export interface PolicyInput extends PolicySource {
  management?: Management
}

export interface Validation {
  ok: boolean
  rego?: string
  hash?: string
  errors: Diagnostic[]
  warnings: Diagnostic[]
}

export interface Evaluation {
  ok: boolean
  allow?: boolean
  errors?: Diagnostic[]
}

export interface Preset {
  id: string
  title: string
  description: string
  kind: PolicyKind
  source: string
}

export const SCOPES = ["sessions:read", "sessions:write", "policies:read", "policies:write"] as const
export type Scope = (typeof SCOPES)[number]

export interface Token {
  id: string
  name: string
  scopes: Scope[]
  created: string
  expires: string
  last_used?: string
}

export type CreatedToken = Token & { token: string } // the secret, in this answer and nowhere else

export interface NewToken {
  name: string
  scopes: Scope[]
  expires_in_days?: number
}

// A session as the backend answers once policies are on. Absent `policy`
// means the deployment has the feature off.
export type PolicySession = Session & { policy?: PolicySummary }

export interface NewSession {
  name?: string
  policy?: PolicyInput
}

// An error answer with the contract's extra fields: `errors` and `warnings`
// of a policy that does not validate (422), `managed_url` of a mode refusal (409).
export class PolicyApiError extends ApiError {
  constructor(
    status: number,
    message: string,
    public errors: Diagnostic[] = [],
    public warnings: Diagnostic[] = [],
    public managedUrl?: string,
  ) {
    super(status, message)
  }
}

// What a write of the policy came back as: 200 is in force on every replica,
// 202 is saved and still loading.
export interface Saved {
  policy: Policy
  inForce: boolean
}

const isJson = (res: Response) => /^application\/([\w.-]+\+)?json\b/i.test(res.headers.get("Content-Type") ?? "")

type Fetch = typeof fetch

export function createPolicyApi(fetchImpl: Fetch = (input, init) => fetch(input, init)) {
  async function send(method: string, path: string, body?: unknown, extra?: Record<string, string>) {
    const headers = new Headers({ Accept: "application/json", ...extra })
    if (body !== undefined) headers.set("Content-Type", "application/json")
    // Signed-out detection as in api.ts: a 401, an opaque redirect, or HTML.
    const res = await fetchImpl(path, {
      method,
      headers,
      credentials: "same-origin",
      redirect: "manual",
      body: body === undefined ? undefined : JSON.stringify(body),
    })
    if (res.type === "opaqueredirect" || res.status === 401) throw new SignedOutError()
    if (!res.ok) {
      let detail: { error?: string; errors?: Diagnostic[]; warnings?: Diagnostic[]; managed_url?: string } = {}
      try {
        detail = (await res.json()) ?? {}
      } catch {}
      throw withRefusal(
        new PolicyApiError(
          res.status,
          detail.error ?? `${res.status} ${res.statusText}`,
          Array.isArray(detail.errors) ? detail.errors : [],
          Array.isArray(detail.warnings) ? detail.warnings : [],
          detail.managed_url,
        ),
        detail,
      )
    }
    return res
  }

  async function call<T>(method: string, path: string, body?: unknown, extra?: Record<string, string>): Promise<T> {
    const res = await send(method, path, body, extra)
    if (res.status === 204) return undefined as T
    if (!isJson(res)) throw new SignedOutError()
    return (await res.json()) as T
  }

  async function saved(method: string, path: string, body?: unknown, extra?: Record<string, string>): Promise<Saved> {
    const res = await send(method, path, body, extra)
    if (!isJson(res)) throw new SignedOutError()
    return { policy: (await res.json()) as Policy, inForce: res.status !== 202 }
  }

  // Rejects with a 404 before any request for a malformed id, as api.ts does.
  async function policyPath(id: string, suffix = ""): Promise<string> {
    if (!isSessionId(id)) throw new PolicyApiError(404, "session not found")
    return `/api/sessions/${encodeURIComponent(id)}/policy${suffix}`
  }

  return {
    createSession: (body: NewSession) => call<PolicySession>("POST", "/api/sessions", body),
    getPolicy: async (id: string) => call<Policy>("GET", await policyPath(id)),
    // `ifMatch` is the version the edit started from; the ETag is that version, quoted.
    putPolicy: async (id: string, input: PolicyInput, ifMatch?: number) =>
      saved("PUT", await policyPath(id), input, ifMatch === undefined ? undefined : { "If-Match": `"${ifMatch}"` }),
    resetPolicy: async (id: string) => saved("DELETE", await policyPath(id)),
    setManagement: async (id: string, management: Management) =>
      call<Policy>("PUT", await policyPath(id, "/management"), management),
    validate: (source: PolicySource) => call<Validation>("POST", "/api/policies/validate", source),
    evaluate: (source: PolicySource, input: unknown) =>
      call<Evaluation>("POST", "/api/policies/evaluate", { ...source, input }),
    presets: () => call<Preset[]>("GET", "/api/policy-presets"),
    listTokens: () => call<Token[]>("GET", "/api/tokens"),
    createToken: (body: NewToken) => call<CreatedToken>("POST", "/api/tokens", body),
    revokeToken: (tokenId: string) => call<void>("DELETE", `/api/tokens/${encodeURIComponent(tokenId)}`),
  }
}

export type PolicyApi = ReturnType<typeof createPolicyApi>

export const policyApi = createPolicyApi()

// Resolves to null where the deployment does not have the endpoint (the
// backend ships the feature behind a flag): the caller then shows nothing.
// A sign-out still rejects.
export async function ifAvailable<T>(request: Promise<T>): Promise<T | null> {
  try {
    return await request
  } catch (e) {
    if (e instanceof ApiError) return null
    throw e
  }
}

let tokensProbe: Promise<boolean> | undefined

// Whether this deployment serves /tokens. Asked once per page load.
export function tokensAvailable(apiImpl: PolicyApi = policyApi): Promise<boolean> {
  tokensProbe ??= ifAvailable(apiImpl.listTokens()).then(
    list => list !== null,
    () => false,
  )
  return tokensProbe
}

// For tests, which start a different backend each time.
export function forgetTokensProbe() {
  tokensProbe = undefined
}

export const KIND_LABEL: Record<PolicyKind, string> = { rego: "Rego" }

// The state line of the Policy tab, from `state` and `loaded`.
export function policyStatus(p: Pick<Policy, "state" | "loaded" | "hash">): string {
  const { replicas, total } = p.loaded ?? {}
  const count = replicas !== undefined && total !== undefined ? ` (${replicas}/${total})` : ""
  switch (p.state) {
    case "ready":
      return `in force${count}`
    case "loading":
      return `loading${count}`
    case "invalid":
      // A policy that never compiled has no hash: nothing is in force but the refusal.
      return p.hash ? "does not compile; the previous policy is still in force" : "does not compile"
    case "unsupported":
      return "not supported"
  }
}

// The one-line summary on the session's page and in the list: "Rego, v3, in force".
export function policySummaryLine(p: PolicySummary): string {
  if (p.state === "unsupported") return "none (created before policies)"
  const parts = [p.kind ? KIND_LABEL[p.kind] : "", p.version !== undefined ? `v${p.version}` : "", policyStatus(p)]
  return parts.filter(Boolean).join(", ")
}

export const isManagedAsCode = (p?: PolicySummary | null) => p?.management?.mode === "iac"

// "ui" or "token:<name>", as the backend records it.
export function updatedByLabel(by?: string): string {
  if (!by) return ""
  if (by === "ui") return "in the UI"
  if (by.startsWith("token:")) return `by token "${by.slice("token:".length)}"`
  return `by ${by}`
}

const UNRESTRICTED = "package browserjs.policy import rego.v1 allow_tool_call := true".replace(/\s+/g, "")

// True for the `unrestricted` preset: the module that, with its comments and
// whitespace removed, is the package, the import and `allow_tool_call := true`.
// Anything else may restrict something, and is not called unrestricted.
export function isUnrestricted(p: Pick<Policy, "kind" | "source">): boolean {
  if (p.kind !== "rego" || !p.source) return false
  return p.source.replace(/#.*$/gm, "").replace(/\s+/g, "") === UNRESTRICTED
}

// The link of a policy managed as code: https only, as the backend requires.
export function managedUrlError(url: string): string {
  const value = url.trim()
  if (!value) return "Enter the link to where the policy is managed."
  try {
    if (new URL(value).protocol !== "https:") return "The link must start with https://."
  } catch {
    return "Enter a valid link, starting with https://."
  }
  return ""
}

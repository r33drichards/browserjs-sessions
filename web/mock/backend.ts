// A stand-in for the backend, answering as docs/contracts/policy/backend-api.yaml
// says: for development (`npm run dev:mock`) and for the component tests.
// State is in memory. It is the cookie caller: it may write a policy in
// `editor` mode and is refused in `iac` mode.
//
// What it does not do: compile or run Rego. A policy is a Rego module and
// this file has no Rego engine, so:
//
//   validate   checks three things: the package clause, a call to http.send
//              (an error with a position), and a module with no
//              allow_tool_call (a warning). It also warns, without a
//              position, when a module that looks at the browser's
//              operations names the exec server or desktop_execute, the way
//              the real check warns about a restriction that another tool
//              walks around. Everything else is "valid".
//   evaluate   a server and tool the platform does not know are refused,
//              as a session refuses them. Then, when the source is one of
//              the presets and the input is one of that preset's cases
//              (examples/<id>.cases.json), the answer is the case's. For
//              anything else it is a placeholder, not the policy's answer:
//              the unrestricted module allows, and every other module allows
//              browser_execute and nothing else.

import { createBillingMock } from "./billing"

export interface MockRequest {
  method: string
  path: string // with the query, e.g. /api/sessions?all=1
  headers?: Record<string, string> // lower-case names
  body?: unknown
}

export interface MockResponse {
  status: number
  headers?: Record<string, string>
  body?: unknown // JSON; undefined for no body
  text?: string // a non-JSON body
}

interface Diagnostic {
  row?: number
  col?: number
  code: string
  message: string
}

interface Source {
  kind?: "rego" // the only kind; may be left out
  source: string
}

interface Management {
  mode: "editor" | "iac"
  managed_url?: string
}

interface StoredPolicy extends Source {
  kind: "rego"
  version: number
  management: Management
  state: "ready" | "loading" | "invalid"
  rego: string // the last source that compiled
  errors: Diagnostic[]
  updated: string
  updated_by: string
  loads: number // GETs left before `loading` becomes `ready`
}

interface StoredSession {
  id: string
  name: string
  owner: string
  state: string
  created: string
  policy?: StoredPolicy // undefined with `unsupported`: a session from before policies
  unsupported?: boolean
  stoppedBy?: string
  stateSaved?: boolean // asleep with a snapshot to wake from
  draining?: string
  deleteAfter?: string
}

export interface PresetCase {
  name: string
  input: unknown
  allow: boolean
}

export interface Preset {
  id: string
  title: string
  description: string
  kind: "rego"
  source: string // the contract's examples/<id>.rego
  cases: PresetCase[] // examples/<id>.cases.json
}

export interface MockOptions {
  presets: Preset[] // `unrestricted` first
  policies?: boolean // false: a backend with the feature off (default true)
  tokens?: boolean // false: a backend without /tokens (default true)
  seed?: boolean // sessions in every policy state (default true)
  snapshots?: boolean // false: a cluster without Pod Snapshots, where a sleep saves no state (default true)
  billing?: string // a scenario of mock/billing.ts; absent or "off": a backend with billing off
  checkoutPolls?: number
  now?: () => Date
}

// The tools a policy is asked about, by server. Any other pair is refused.
const TOOLS: Record<string, string[]> = { browser: ["browser_execute", "desktop_execute"], exec: ["exec", "stream_logs", "search_logs", "kill"] }
const UNRESTRICTED = "packagebrowserjs.policyimportrego.v1allow_tool_call:=true"
const SCOPES = ["sessions:read", "sessions:write", "policies:read", "policies:write"]
const ME = { email: "you@example.com", name: "You", admin: false }
const SESSION_ID = /^s-([a-z2-7]{10}|[a-z0-9]{5})$/

const json = (status: number, body: unknown, headers?: Record<string, string>): MockResponse => ({ status, body, headers })
const error = (status: number, message: string, extra: object = {}) => json(status, { error: message, ...extra })
// What Go's mux answers for a path nobody registered.
const notRouted = (): MockResponse => ({ status: 404, text: "404 page not found\n", headers: { "Content-Type": "text/plain; charset=utf-8" } })

function position(source: string, index: number) {
  const before = source.slice(0, Math.max(index, 0))
  const row = before.split("\n").length
  return { row, col: before.length - before.lastIndexOf("\n") }
}

const withoutComments = (source: string) => source.replace(/#.*$/gm, "")

// Key order aside, the same JSON value.
function sameJson(a: unknown, b: unknown): boolean {
  if (a === b) return true
  if (typeof a !== "object" || typeof b !== "object" || a === null || b === null) return false
  if (Array.isArray(a) !== Array.isArray(b)) return false
  const [ka, kb] = [Object.keys(a), Object.keys(b)]
  return ka.length === kb.length && ka.every(k => k in b && sameJson((a as any)[k], (b as any)[k]))
}

export function createMockBackend(options: MockOptions) {
  const { presets, policies = true, tokens: tokensOn = true, seed = true, snapshots = true, now = () => new Date() } = options
  const sessions = new Map<string, StoredSession>()
  const tokens: { id: string; name: string; scopes: string[]; created: string; expires: string; last_used?: string }[] = []
  let counter = 0

  function validate({ source }: Source): { ok: boolean; rego?: string; hash?: string; errors: Diagnostic[]; warnings: Diagnostic[] } {
    const errors: Diagnostic[] = []
    const warnings: Diagnostic[] = []
    const code = withoutComments(source)
    if (!/^\s*package\s+browserjs\.policy\s*$/m.test(source))
      errors.push({ row: 1, col: 1, code: "rego_package", message: "the module must be package browserjs.policy" })
    const sent = source.indexOf("http.send")
    if (sent >= 0) errors.push({ ...position(source, sent), code: "rego_type_error", message: "undefined function http.send" })
    if (!/\ballow_tool_call\b/.test(code))
      warnings.push({ row: 1, col: 1, code: "no_allow_rule", message: "no allow_tool_call rule: every call is refused" })
    // A stand-in for the real check's bypass warnings, which have no position.
    if (/\.arguments\.operations\b/.test(code)) {
      if (code.includes('"desktop_execute"'))
        warnings.push({
          code: "browser_bypass_desktop",
          message:
            "this policy restricts what browser_execute may do and allows desktop_execute, which can type into the address bar or DevTools and so do in the browser what the policy refuses",
        })
      if (code.includes('"exec"'))
        warnings.push({
          code: "browser_bypass_shell",
          message:
            "this policy restricts what browser_execute may do and allows the shell, where a command can reach the browser's own control ports and so do in the browser what the policy refuses",
        })
    }
    const ok = errors.length === 0
    return { ok, ...(ok ? { rego: source, hash: hash(source) } : {}), errors, warnings }
  }

  // The 400 of a request whose policy is not a Rego source.
  const malformed = (input: any): MockResponse | undefined => {
    if (input?.kind !== undefined && input.kind !== "rego") return error(400, 'kind must be "rego"')
    if (typeof input?.source !== "string") return error(400, "source must be a string")
    return undefined
  }

  // See the top of this file: a preset's cases, or a placeholder.
  function evaluate(source: string, input: any): boolean {
    if (!TOOLS[input?.server]?.includes(input?.tool)) return false
    const expected = presets.find(p => p.source === source)?.cases.find(c => sameJson(c.input, input))
    if (expected) return expected.allow
    if (withoutComments(source).replace(/\s+/g, "") === UNRESTRICTED) return true
    return input.tool === "browser_execute"
  }

  function hash(source: string): string {
    let h = 2166136261
    for (let i = 0; i < source.length; i++) h = Math.imul(h ^ source.charCodeAt(i), 16777619)
    return "sha256:" + (h >>> 0).toString(16).padStart(8, "0").repeat(8)
  }

  function store(input: Source, management: Management, by: string, previous?: StoredPolicy, loading = false): StoredPolicy {
    const verdict = validate(input)
    return {
      kind: "rego",
      source: input.source,
      version: (previous?.version ?? 0) + 1,
      management,
      state: loading ? "loading" : "ready",
      rego: verdict.rego ?? "",
      errors: [],
      updated: now().toISOString(),
      updated_by: by,
      loads: loading ? 2 : 0,
    }
  }

  const summary = (s: StoredSession) => {
    if (s.unsupported) return { state: "unsupported" }
    const p = s.policy!
    return { kind: p.kind, version: p.version, hash: hash(p.source), state: p.state, management: p.management }
  }

  const sessionView = (s: StoredSession) => ({
    id: s.id,
    name: s.name,
    owner: s.owner,
    state: s.state,
    created: s.created,
    mcp_url: `https://sessions.example.com/${s.id}/mcp`,
    ...(s.stateSaved && s.state !== "running" && s.state !== "starting" ? { stateSaved: true } : {}),
    ...(policies ? { policy: summary(s) } : {}),
    ...billing.view(s),
  })

  const policyView = (s: StoredSession) => {
    const p = s.policy!
    const total = 2
    return {
      ...summary(s),
      source: p.source,
      rego: p.rego,
      errors: p.errors,
      warnings: validate(p).warnings,
      loaded: { replicas: p.state === "ready" ? total : p.state === "loading" ? 1 : 0, total },
      updated: p.updated,
      updated_by: p.updated_by,
    }
  }

  function addSession(name: string, policy?: StoredPolicy, extra: Partial<StoredSession> = {}): StoredSession {
    // Five characters, as a session taken from the warm pool has.
    const id = `s-${(counter++).toString(36).padStart(5, "a")}`
    const s: StoredSession = { id, name, owner: ME.email, state: "running", created: now().toISOString(), policy, ...extra }
    sessions.set(id, s)
    return s
  }

  const seedSessions = () => {
    sessions.clear()
    if (!seed) return
    const [unrestricted, ...rest] = presets
    const pick = (id: string) => presets.find(p => p.id === id) ?? rest[0] ?? unrestricted
    addSession("research", store(pick("one-site"), { mode: "editor" }, "ui"))
    addSession("scratch", store(unrestricted, { mode: "editor" }, "ui"))
    addSession(
      "ci-runner",
      store(pick("observe-only"), { mode: "iac", managed_url: "https://github.com/me/infra/blob/main/browserjs/main.tf" }, "token:ci"),
    )
    addSession("just-saved", store(pick("no-scripting"), { mode: "editor" }, "ui", undefined, true))
    // A source that stopped compiling: the one before it is still in force.
    const broken = addSession("broken", store(pick("form-filling"), { mode: "editor" }, "ui"))
    const lines = broken.policy!.source.replace(/\n$/, "").split("\n")
    broken.policy!.source += "\nallow_tool_call if http.send({})\n"
    broken.policy!.state = "invalid"
    broken.policy!.errors = [{ row: lines.length + 2, col: 20, code: "rego_type_error", message: "undefined function http.send" }]
    addSession("from-before", undefined, { unsupported: true, state: "asleep", stateSaved: true })
  }
  const billing = createBillingMock({
    scenario: options.billing ?? "off",
    email: ME.email,
    now,
    sessions,
    reseed: seedSessions,
    checkoutPolls: options.checkoutPolls,
  })

  function handle(req: MockRequest): MockResponse {
    const [path, query = ""] = req.path.split("?")
    const method = req.method.toUpperCase()
    const body = (req.body ?? {}) as any
    const parts = path.split("/").filter(Boolean) // ["api", ...]
    if (parts[0] !== "api") return notRouted()
    const route = `${method} /${parts.slice(1).join("/")}`

    if (route === "GET /me") return json(200, ME)

    if (route === "GET /sessions") {
      void query
      return json(200, [...sessions.values()].map(sessionView))
    }
    if (parts[1] === "billing" || parts[1] === "account") return billing.handle(method, parts, query, body) ?? notRouted()

    if (route === "POST /sessions") {
      const refused = billing.refuse("create")
      if (refused) return refused
      if (sessions.size >= 12) return error(409, "session limit reached")
      let policy: StoredPolicy | undefined
      if (policies) {
        const input: Source = body.policy ?? presets[0]
        const management: Management = body.policy?.management ?? { mode: "editor" }
        const bad = malformed(input)
        if (bad) return bad
        const verdict = validate(input)
        if (!verdict.ok) return error(422, "the policy does not validate", { errors: verdict.errors, warnings: verdict.warnings })
        if (management.mode === "iac" && !/^https:\/\//.test(management.managed_url ?? ""))
          return error(400, "managed_url must be an https URL when mode is iac")
        policy = store(input, management, "ui", undefined, true)
      }
      const s = addSession(String(body.name || `session-${counter}`), policy, { state: policies ? "starting" : "running" })
      return json(201, sessionView(s))
    }

    if (parts[1] === "sessions" && parts[2]) {
      const s = SESSION_ID.test(parts[2]) ? sessions.get(parts[2]) : undefined
      if (!s) return error(404, "session not found")
      const rest = parts.slice(3).join("/")

      if (rest === "") {
        if (method === "GET") {
          // A new session is `starting` until its policy is loaded.
          if (s.state === "starting" && (!s.policy || s.policy.state === "ready")) s.state = "running"
          return json(200, sessionView(s))
        }
        if (method === "PATCH") {
          if (typeof body.name === "string") s.name = body.name
          if (body.action === "stop") Object.assign(s, { state: "stopped", stoppedBy: "user", stateSaved: false })
          if (body.action === "resume") {
            const refused = billing.refuse("resume")
            if (refused) return refused
            Object.assign(s, { state: "running", stoppedBy: undefined })
          }
          return json(200, sessionView(s))
        }
        if (method === "DELETE") {
          sessions.delete(s.id)
          return { status: 204 }
        }
      }
      // A user's sleep: the snapshot, then asleep. Asleep already is a no-op.
      if (rest === "sleep" && method === "POST") {
        if (s.state === "asleep") return json(200, sessionView(s))
        if (s.state === "starting") return error(409, "session is still starting; put it to sleep once it is running")
        if (s.state !== "running") return error(409, `session is ${s.state}, with no running state to save; wake it to start it fresh`)
        Object.assign(s, { state: "asleep", stoppedBy: "sleep", stateSaved: snapshots })
        return json(200, sessionView(s))
      }
      // PATCH's resume, as a route: from the snapshot if there is one.
      if (rest === "wake" && method === "POST") {
        if (s.state === "running" || s.state === "starting") return json(200, sessionView(s))
        const refused = billing.refuse("resume")
        if (refused) return refused
        Object.assign(s, { state: "running", stoppedBy: undefined })
        return json(200, sessionView(s))
      }
      if (rest === "vnc-ticket" && method === "POST") return error(503, "the mock backend has no browser to show")
      if (rest === "files" && method === "GET") return json(200, { files: [], max_bytes: 1 << 20 })

      if (!policies) return notRouted()

      if (rest === "policy") {
        if (s.unsupported) return error(409, "this session predates policies")
        const p = s.policy!
        if (method === "GET") {
          if (p.state === "loading" && --p.loads <= 0) p.state = "ready"
          return json(200, policyView(s), { ETag: `"${p.version}"` })
        }
        if (method === "PUT" || method === "DELETE") {
          if (p.management.mode === "iac")
            return error(409, "this policy is managed externally", { managed_url: p.management.managed_url })
          const ifMatch = req.headers?.["if-match"]
          if (ifMatch !== undefined && ifMatch !== `"${p.version}"`) return error(412, "the policy has changed since that version")
          const input: Source = method === "DELETE" ? presets[0] : { kind: body.kind, source: body.source }
          const bad = malformed(input)
          if (bad) return bad
          const verdict = validate(input)
          if (!verdict.ok) return error(422, "the policy does not validate", { errors: verdict.errors, warnings: verdict.warnings })
          // A request that changes nothing is 200 and does not raise the version.
          if (p.source === input.source) return json(200, policyView(s))
          // A policy with a comment that says "slow" stays `loading` for a while: the 202 path.
          const slow = /#.*\bslow\b/.test(input.source)
          s.policy = store(input, body.management ?? { mode: "editor" }, "ui", p, slow)
          return json(slow ? 202 : 200, policyView(s), { ETag: `"${s.policy.version}"` })
        }
      }
      if (rest === "policy/management" && method === "PUT") {
        if (s.unsupported) return error(409, "this session predates policies")
        if (body.mode !== "editor" && body.mode !== "iac") return error(400, "mode must be editor or iac")
        if (body.mode === "iac" && !/^https:\/\//.test(body.managed_url ?? "")) return error(400, "managed_url must be an https URL when mode is iac")
        s.policy!.management = body.mode === "iac" ? { mode: "iac", managed_url: body.managed_url } : { mode: "editor" }
        return json(200, policyView(s))
      }
      return notRouted()
    }

    if (policies) {
      if (route === "POST /policies/validate") return malformed(body) ?? json(200, validate(body))
      if (route === "POST /policies/evaluate") {
        const bad = malformed(body)
        if (bad) return bad
        const verdict = validate(body)
        if (!verdict.ok) return json(200, { ok: false, errors: verdict.errors })
        return json(200, { ok: true, allow: evaluate(body.source, body.input) })
      }
      if (route === "GET /policy-presets")
        return json(200, presets.map(({ id, title, description, kind, source }) => ({ id, title, description, kind, source })))
    }

    if (tokensOn && parts[1] === "tokens") {
      if (route === "GET /tokens") return json(200, [...tokens].reverse())
      if (route === "POST /tokens") {
        if (tokens.length >= 20) return error(409, "you already have 20 tokens")
        const scopes: string[] = Array.isArray(body.scopes) ? body.scopes : []
        if (!body.name || scopes.length === 0 || scopes.some(s => !SCOPES.includes(s))) return error(400, "a token needs a name and scopes")
        const days = body.expires_in_days ?? 90
        const id = `tok${(counter++).toString(36).padStart(9, "a")}`
        const token = {
          id,
          name: String(body.name),
          scopes,
          created: now().toISOString(),
          expires: new Date(now().getTime() + days * 86_400_000).toISOString(),
        }
        tokens.push(token)
        return json(201, { ...token, token: `bjs_${id}_${"m0ck".repeat(10)}abc` })
      }
      if (method === "DELETE" && parts[2]) {
        const at = tokens.findIndex(t => t.id === parts[2])
        if (at >= 0) tokens.splice(at, 1)
        return { status: 204 }
      }
    }

    return notRouted()
  }

  // The backend as a `fetch`, for tests.
  const fetch = async (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const headers: Record<string, string> = {}
    new Headers(init?.headers).forEach((value, name) => (headers[name.toLowerCase()] = value))
    const res = handle({
      method: init?.method ?? "GET",
      path: String(input),
      headers,
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
    })
    const text = res.text ?? (res.body === undefined ? null : JSON.stringify(res.body))
    return new Response(text, { status: res.status, headers: { "Content-Type": "application/json", ...res.headers } })
  }

  return { handle, fetch, sessions, billing }
}

export type MockBackend = ReturnType<typeof createMockBackend>

// The titles that are not their id with spaces.
const TITLES: Record<string, string> = { "read-only-shell": "Read-only shell" }

// "No scripting" of "no-scripting".
function presetTitle(id: string): string {
  const words = id.replace(/-/g, " ")
  return TITLES[id] ?? words[0].toUpperCase() + words.slice(1)
}

// The comment a preset begins with, as one line.
export function presetDescription(source: string): string {
  const words: string[] = []
  for (const line of source.split("\n")) {
    if (!line.startsWith("#")) break
    words.push(...line.slice(1).split(/\s+/).filter(Boolean))
  }
  return words.join(" ")
}

// The presets as the backend serves them: the contract's examples/<id>.rego,
// `unrestricted` first, each with its examples/<id>.cases.json.
export function presetsFromExamples(files: Record<string, string>): Preset[] {
  const file = (name: string) => files[Object.keys(files).find(n => n.split("/").pop() === name) ?? ""]
  const ids = Object.keys(files)
    .map(name => /([^/]+)\.rego$/.exec(name)?.[1])
    .filter((id): id is string => !!id)
    .sort((a, b) => (a === "unrestricted" ? -1 : b === "unrestricted" ? 1 : a < b ? -1 : 1))
  return ids.map(id => {
    const source = file(`${id}.rego`)
    const cases = file(`${id}.cases.json`)
    return { id, title: presetTitle(id), description: presetDescription(source), kind: "rego", source, cases: cases ? JSON.parse(cases) : [] }
  })
}

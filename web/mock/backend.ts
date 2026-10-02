// A stand-in for the backend, answering as docs/contracts/policy/backend-api.yaml
// says: for development (`npm run dev:mock`) and for the component tests.
// State is in memory. It is the cookie caller: it may write a policy in
// `editor` mode and is refused in `iac` mode.
//
// What it does not do: compile Rego. A JSON policy is checked against the
// rules of the schema that matter to the UI and evaluated by the JSON
// format's own semantics; its "generated Rego" is the contract's example when
// the policy is one of the presets, and a placeholder otherwise. A Rego
// policy is "valid" when it has the package and an allow_tool_call rule.

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
  kind: "json" | "rego"
  source: string
}

interface Management {
  mode: "editor" | "iac"
  managed_url?: string
}

interface StoredPolicy extends Source {
  version: number
  management: Management
  state: "ready" | "loading" | "invalid"
  rego: string
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
  draining?: string
  deleteAfter?: string
}

export interface Preset extends Source {
  id: string
  title: string
  description: string
  rego: string // the contract's examples/<id>.rego
}

export interface MockOptions {
  presets: Preset[] // `unrestricted` first
  schema: unknown // json-policy.schema.json
  policies?: boolean // false: a backend with the feature off (default true)
  tokens?: boolean // false: a backend without /tokens (default true)
  seed?: boolean // sessions in every policy state (default true)
  billing?: string // a scenario of mock/billing.ts; absent or "off": a backend with billing off
  checkoutPolls?: number
  now?: () => Date
}

const OPERATIONS = ["click", "evaluate", "navigate", "press", "screenshot", "select", "setContent", "setViewport", "type", "url", "wait"]
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

function closest(word: string): string | undefined {
  return OPERATIONS.find(op => op.toLowerCase().startsWith(word.toLowerCase().slice(0, 3)))
}

export function createMockBackend(options: MockOptions) {
  const { presets, schema, policies = true, tokens: tokensOn = true, seed = true, now = () => new Date() } = options
  const sessions = new Map<string, StoredSession>()
  const tokens: { id: string; name: string; scopes: string[]; created: string; expires: string; last_used?: string }[] = []
  let counter = 0

  const presetRego = (source: string) => {
    try {
      const wanted = JSON.stringify(JSON.parse(source))
      return presets.find(p => p.kind === "json" && JSON.stringify(JSON.parse(p.source)) === wanted)?.rego
    } catch {
      return undefined
    }
  }

  function validate({ kind, source }: Source): { ok: boolean; rego?: string; hash?: string; errors: Diagnostic[]; warnings: Diagnostic[] } {
    const errors: Diagnostic[] = []
    const warnings: Diagnostic[] = []
    let rego: string | undefined
    if (kind === "json") {
      let doc: any
      try {
        doc = JSON.parse(source)
      } catch (e) {
        const at = /position (\d+)/.exec(String((e as Error).message))
        errors.push({ ...position(source, at ? Number(at[1]) : 0), code: "json_parse_error", message: (e as Error).message })
      }
      if (doc !== undefined) {
        if (doc === null || typeof doc !== "object" || Array.isArray(doc)) {
          errors.push({ row: 1, col: 1, code: "schema_error", message: "a policy is a JSON object" })
        } else {
          if (doc.version !== 1) errors.push({ ...position(source, source.indexOf('"version"')), code: "schema_error", message: "version must be 1" })
          for (const key of Object.keys(doc)) {
            if (!["version", "description", "allow", "deny"].includes(key))
              errors.push({ ...position(source, source.indexOf(`"${key}"`)), code: "schema_error", message: `unknown property "${key}"` })
          }
          const named: string[] = [
            ...(doc.allow?.operations ?? []),
            ...(doc.deny?.operations ?? []),
            ...(doc.allow?.rules ?? []).map((r: any) => r?.operation),
          ]
          for (const op of named) {
            if (op === "*" || OPERATIONS.includes(op)) continue
            const hint = closest(String(op))
            errors.push({
              ...position(source, source.indexOf(`"${op}"`)),
              code: "unknown_operation",
              message: `unknown operation "${op}"${hint ? `; did you mean "${hint}"?` : ""}`,
            })
          }
          if (!doc.allow) warnings.push({ row: 1, col: 1, code: "allows_nothing", message: "this policy allows nothing: it has no allow section" })
        }
      }
      if (errors.length === 0) {
        rego =
          presetRego(source) ??
          "# Generated from a browserjs JSON policy (version 1). Edit the JSON, not this file.\n" +
            "package browserjs.policy\n\nimport rego.v1\n\n# (mock backend: the real translation comes from the policy operator)\n"
      }
    } else {
      if (!/^\s*package\s+browserjs\.policy\s*$/m.test(source))
        errors.push({ row: 1, col: 1, code: "rego_package", message: "the module must be package browserjs.policy" })
      const sent = source.indexOf("http.send")
      if (sent >= 0) errors.push({ ...position(source, sent), code: "rego_type_error", message: "undefined function http.send" })
      if (!/\ballow_tool_call\b/.test(source))
        warnings.push({ row: 1, col: 1, code: "no_allow_rule", message: "no allow_tool_call rule: every call is refused" })
      if (errors.length === 0) rego = source
    }
    const ok = errors.length === 0
    return { ok, ...(ok ? { rego, hash: hash(source) } : {}), errors, warnings }
  }

  function hash(source: string): string {
    let h = 2166136261
    for (let i = 0; i < source.length; i++) h = Math.imul(h ^ source.charCodeAt(i), 16777619)
    return "sha256:" + (h >>> 0).toString(16).padStart(8, "0").repeat(8)
  }

  // The JSON format's semantics (json-policy.schema.json's description).
  function evaluateJson(doc: any, input: any): boolean {
    if (input?.server !== "browser" || input?.tool !== "browser_execute") return false
    const ops = input?.arguments?.operations
    if (!Array.isArray(ops)) return false
    const denied: string[] = doc.deny?.operations ?? []
    const allowed: string[] = doc.allow?.operations ?? []
    const rules: any[] = doc.allow?.rules ?? []
    return ops.every(op => {
      if (!OPERATIONS.includes(op?.type) || denied.includes(op.type)) return false
      if (allowed.includes("*") || allowed.includes(op.type)) return true
      return rules.some(rule => rule.operation === op.type && Object.entries(rule.constraints ?? {}).every(([name, c]) => passes(c, op.params?.[name])))
    })
  }

  function passes(c: any, value: unknown): boolean {
    if (value === undefined) return false
    if (c.min !== undefined && !(typeof value === "number" && value >= c.min)) return false
    if (c.max !== undefined && !(typeof value === "number" && value <= c.max)) return false
    if (c.max_length !== undefined && !(typeof value === "string" && value.length <= c.max_length)) return false
    if (c.pattern !== undefined && !(typeof value === "string" && new RegExp(c.pattern).test(value))) return false
    if (c.allowed !== undefined && !c.allowed.includes(value)) return false
    if (c.hosts !== undefined || c.schemes !== undefined) {
      let url: URL
      try {
        url = new URL(String(value))
      } catch {
        return false
      }
      const schemes: string[] = c.schemes ?? ["http", "https"]
      if (!schemes.includes(url.protocol.replace(":", ""))) return false
      const host = url.hostname.toLowerCase()
      if (c.hosts && !c.hosts.some((h: string) => (h.startsWith("*.") ? host.endsWith(h.slice(1)) : host === h))) return false
    }
    return true
  }

  function store(input: Source, management: Management, by: string, previous?: StoredPolicy, loading = false): StoredPolicy {
    const verdict = validate(input)
    return {
      kind: input.kind,
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
      warnings: [],
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
      store(
        { kind: "rego", source: pick("observe-only").rego.replace(/^# Generated.*\n/, "") },
        { mode: "iac", managed_url: "https://github.com/me/infra/blob/main/browserjs/main.tf" },
        "token:ci",
      ),
    )
    addSession("just-saved", store(pick("no-scripting"), { mode: "editor" }, "ui", undefined, true))
    const broken = addSession("broken", store(pick("form-filling"), { mode: "editor" }, "ui"))
    broken.policy!.state = "invalid"
    broken.policy!.errors = [{ row: 4, col: 21, code: "rego_compile_error", message: "the policy no longer compiles under the current capabilities" }]
    addSession("from-before", undefined, { unsupported: true, state: "asleep" })
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
          if (body.action === "stop") s.state = "stopped"
          if (body.action === "resume") {
            const refused = billing.refuse("resume")
            if (refused) return refused
            s.state = "running"
          }
          return json(200, sessionView(s))
        }
        if (method === "DELETE") {
          sessions.delete(s.id)
          return { status: 204 }
        }
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
          const verdict = validate(input)
          if (!verdict.ok) return error(422, "the policy does not validate", { errors: verdict.errors, warnings: verdict.warnings })
          // A request that changes nothing is 200 and does not raise the version.
          if (p.kind === input.kind && p.source === input.source) return json(200, policyView(s))
          // A policy whose description says "slow" stays `loading` for a while: the 202 path.
          const slow = /"description":\s*"[^"]*slow/.test(input.source)
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
      if (route === "POST /policies/validate") return json(200, validate(body))
      if (route === "POST /policies/evaluate") {
        const verdict = validate(body)
        if (!verdict.ok) return json(200, { ok: false, errors: verdict.errors })
        // Rego is not evaluated here: a module with an allow rule allows.
        const allow = body.kind === "json" ? evaluateJson(JSON.parse(body.source), body.input) : /\ballow_tool_call\b/.test(body.source)
        return json(200, { ok: true, allow })
      }
      if (route === "GET /policy-schema.json") return json(200, schema, { "Content-Type": "application/schema+json" })
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

const TITLES: Record<string, string> = {
  unrestricted: "No restrictions",
  "no-scripting": "No scripting",
  "observe-only": "Observe only",
  "one-site": "One site",
  "form-filling": "Form filling",
}

// The presets as the backend serves them: the contract's examples, `unrestricted` first.
export function presetsFromExamples(files: Record<string, string>): Preset[] {
  const ids = Object.keys(files)
    .map(name => /([^/]+)\.policy\.json$/.exec(name)?.[1])
    .filter((id): id is string => !!id)
    .sort((a, b) => (a === "unrestricted" ? -1 : b === "unrestricted" ? 1 : a.localeCompare(b)))
  return ids.map(id => {
    const source = files[Object.keys(files).find(n => n.endsWith(`${id}.policy.json`))!]
    const rego = files[Object.keys(files).find(n => n.endsWith(`${id}.rego`))!] ?? ""
    return {
      id,
      title: TITLES[id] ?? id,
      description: String(JSON.parse(source).description ?? ""),
      kind: "json" as const,
      source,
      rego,
    }
  })
}

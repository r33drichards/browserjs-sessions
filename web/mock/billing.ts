// The billing half of the mock backend: docs/contracts/billing/backend-api.yaml
// answered from memory, with the proposed numbers of catalogue.yaml. There is
// no Stripe: a Checkout's "payment page" is the app's own return address, so
// buying completes by itself after a poll or two.
//
// A scenario is a name, or several joined with "+" ("low+auto"): each puts
// the account, and the sessions, into one of the states of ui-states.md.
import type { MockResponse } from "./backend"

export interface BillingSession {
  id: string
  name: string
  state: string
  stoppedBy?: string
  draining?: string
  deleteAfter?: string
  size?: string
}

export const BILLING_SCENARIOS = [
  "active", // a subscriber with credit: nothing extra is shown
  "payg", // a card, no subscription
  "new", // signed in, no card, no sessions: the gate
  "terms", // the terms come first
  "no-card", // the card was removed; the sessions are asleep and kept
  "low",
  "grace", // out of credit, sessions still running until sleepAt
  "exhausted",
  "deleting", // out of credit, deletion within the week
  "tomorrow", // deletion tomorrow
  "past-due",
  "recharge-failed",
  "ending", // the plan is cancelled and runs out
  "pending", // the ledger has no status yet
  "stale",
  "meter", // shadow mode
  "blocked",
  "exempt",
  "session-limit",
  "awake-limit",
  "auto", // auto-recharge is available
  "auto-on",
  "live", // real payments: no test-mode note
  "stripe-down", // checkout and the portal answer 502
  "card-used", // the next card saved earns no sign-up credit, by reason
  "prepaid",
  "wallet",
] as const

export interface BillingMockOptions {
  scenario: string
  email: string
  now: () => Date
  sessions: Map<string, BillingSession>
  reseed: () => void // the sessions as first seeded
  checkoutPolls?: number // answers of `open` before a Checkout is complete (default 1)
}

const RATES = { awakeMicrosPerHour: 200_000, diskMicrosPerGBHour: 384, sessionDiskGB: 5 }
const USD = 1_000_000
const DAY = 86_400_000

// The awake rate of each size, and the sizes each plan includes.
const SIZES = [
  { key: "small", awakeMicrosPerHour: 200_000 },
  { key: "medium", awakeMicrosPerHour: 400_000 },
  { key: "large", awakeMicrosPerHour: 800_000 },
]
const PLANS = [
  { key: "starter", name: "Starter", lookupKey: "cu_starter_monthly_v1", amount: 500, creditMicros: 10 * USD, maxSessions: 3, maxAwake: 2, sizes: ["small", "medium"] },
  { key: "pro", name: "Pro", lookupKey: "cu_pro_monthly_v1", amount: 2000, creditMicros: 44 * USD, maxSessions: 10, maxAwake: 4, sizes: ["small", "medium", "large"] },
]
const PACKS = [5, 20, 50].map(n => ({
  key: `credit-${n}`,
  name: `$${n} of credit`,
  lookupKey: `cu_credit_${n}_v1`,
  amount: n * 100,
  creditMicros: n * USD,
  validDays: 365,
}))
const PAYG = { maxSessions: 3, maxAwake: 2, sizes: ["small", "medium"] }
const SIGNUP = { amountMicros: 5 * USD, validDays: 90 }

const CATALOGUE = { currency: "usd", rates: RATES, sizes: SIZES, signupCredit: SIGNUP, payg: PAYG, plans: PLANS, packs: PACKS }

const json = (status: number, body: unknown, headers?: Record<string, string>): MockResponse => ({ status, body, headers })
const refusal = (status: number, code: string, message: string, extra: object = {}) =>
  json(status, { error: message, code, billingUrl: "https://app.example.com/billing", ...extra })

export function createBillingMock(options: BillingMockOptions) {
  const { email, now, sessions, reseed, checkoutPolls = 1 } = options
  const iso = (ms: number) => new Date(now().getTime() + ms).toISOString()
  let b: any
  let stripeDown = false
  let signupOutcome: string = "granted"
  let settle: (() => void)[] = [] // credit that reaches the account at its next read
  const checkouts = new Map<string, { kind: string; item?: any; polls: number; done: boolean; signup?: any }>()
  let counter = 0

  const awake = () => [...sessions.values()].filter(s => s.state === "running" || s.state === "starting")

  function setBalance(balances: { source: string; micros: number; expiresAt?: string }[]) {
    b.balances = balances
    b.balanceMicros = balances.reduce((sum, x) => sum + x.micros, 0)
  }

  function burn() {
    b.burnMicrosPerHour = awake().length * RATES.awakeMicrosPerHour + sessions.size * RATES.sessionDiskGB * RATES.diskMicrosPerGBHour
  }

  function sleepAll(by: string, extra: Partial<BillingSession> = {}) {
    for (const s of sessions.values()) Object.assign(s, { state: "asleep", stoppedBy: by, stateSaved: true }, extra)
  }

  function payg() {
    b.plan = { key: "payg", name: "Pay as you go" }
    b.limits = { ...PAYG }
    delete b.subscription
    delete b.period.planCreditMicros
    setBalance(b.balances.filter((x: any) => x.source !== "plan"))
  }

  function subscribe(plan: (typeof PLANS)[number]) {
    b.plan = { key: plan.key, name: plan.name, amount: plan.amount, creditMicros: plan.creditMicros }
    b.limits = { maxSessions: plan.maxSessions, maxAwake: plan.maxAwake, sizes: plan.sizes }
    b.subscription = { status: "active", renewsAt: b.period.end }
    b.period.planCreditMicros = plan.creditMicros
  }

  function zero() {
    b.level = "exhausted"
    setBalance(b.balances.map((x: any) => ({ ...x, micros: 0 })))
    b.exhaustedAt = iso(-2 * 3_600_000)
  }

  function noCard() {
    b.state = "no_card"
    b.hasPaymentMethod = false
    delete b.card
  }

  const apply: Record<string, () => void> = {
    active: () => {},
    payg,
    new: () => {
      sessions.clear()
      noCard()
      payg()
      setBalance([])
      b.hasCustomer = false
      b.signupCredit = { state: "pending" }
      b.period = { ...b.period, awakeSeconds: 0, awakeMicros: 0, diskMicros: 0 }
    },
    terms: () => {
      apply.new()
      b.state = "terms"
      b.termsRequired = "2026-10-01"
    },
    "no-card": () => {
      noCard()
      sleepAll("payment-method")
    },
    low: () => {
      b.level = "low"
      setBalance([{ source: "purchase", micros: 800_000, expiresAt: iso(200 * DAY) }])
      payg()
    },
    grace: () => {
      zero()
      b.sleepAt = iso(4 * 60_000)
      for (const s of awake().slice(0, 1)) s.draining = "credit"
    },
    exhausted: () => {
      zero()
      sleepAll("credit")
    },
    deleting: () => {
      apply.exhausted()
      payg()
      b.deleteAt = iso(5 * DAY)
      for (const s of sessions.values()) s.deleteAfter = b.deleteAt
    },
    tomorrow: () => {
      apply.deleting()
      b.deleteAt = iso(DAY - 3_600_000)
      for (const s of sessions.values()) s.deleteAfter = b.deleteAt
    },
    "past-due": () => {
      b.subscription = { status: "past_due", renewsAt: b.period.end }
    },
    "recharge-failed": () => {
      b.autoRecharge = {
        available: true,
        enabled: false,
        pack: "credit-20",
        thresholdMicros: 2 * USD,
        monthlyCapCents: 5000,
        chargedCents: 2000,
        lastStatus: "failed",
        disabledReason: "authentication-required",
      }
    },
    ending: () => {
      b.subscription = { status: "active", cancelsAt: b.period.end }
    },
    pending: () => {
      b.ledger = "pending"
      setBalance([])
      delete b.burnMicrosPerHour
    },
    stale: () => {
      b.ledger = "stale"
      b.observedAt = iso(-25 * 60_000)
    },
    meter: () => {
      b.mode = "meter"
      b.payments = "off"
      noCard()
      payg()
      setBalance([])
      b.level = "exhausted"
    },
    blocked: () => {
      b.state = "blocked"
      sleepAll("blocked")
    },
    exempt: () => {
      b.state = "exempt"
      noCard()
      b.state = "exempt"
    },
    "session-limit": () => {
      subscribe(PLANS[0])
    },
    "awake-limit": () => {
      b.limits = { maxSessions: 10, maxAwake: awake().length }
    },
    auto: () => {
      b.autoRecharge = { available: true, enabled: false, chargedCents: 0 }
    },
    "auto-on": () => {
      b.autoRecharge = { available: true, enabled: true, pack: "credit-20", thresholdMicros: 2 * USD, monthlyCapCents: 5000, chargedCents: 2000, lastStatus: "succeeded" }
    },
    live: () => {
      b.payments = "live"
    },
    "stripe-down": () => {
      stripeDown = true
    },
    "card-used": () => void (signupOutcome = "card-used"),
    prepaid: () => void (signupOutcome = "prepaid"),
    wallet: () => void (signupOutcome = "wallet"),
  }

  let on = false

  function set(scenario: string) {
    on = scenario !== "off" && scenario !== ""
    stripeDown = false
    signupOutcome = "granted"
    settle = []
    checkouts.clear()
    reseed()
    if (!on) return
    // Two awake, the rest asleep since they were last used.
    ;[...sessions.values()].forEach((s, i) => {
      if (i >= 2) Object.assign(s, { state: "asleep", stoppedBy: "idle", stateSaved: true })
    })
    const start = iso(-12 * DAY)
    const end = iso(18 * DAY)
    b = {
      mode: "enforce",
      state: "active",
      ledger: "ok",
      hasPaymentMethod: true,
      card: { brand: "visa", last4: "4242", expMonth: 8, expYear: 2028 },
      signupCredit: { state: "granted", amountMicros: SIGNUP.amountMicros },
      level: "ok",
      rates: RATES,
      sizes: SIZES,
      period: { start, end, awakeSeconds: 163_800, awakeMicros: 9_100_000, diskMicros: 3_360_000 },
      payments: "test",
      hasCustomer: true,
      autoRecharge: { available: false, enabled: false },
    }
    setBalance([
      { source: "plan", micros: 31_540_000, expiresAt: end },
      { source: "signup", micros: 1_300_000, expiresAt: iso(78 * DAY) },
      { source: "purchase", micros: 0 },
    ])
    subscribe(PLANS[1])
    for (const name of scenario.split("+")) apply[name.trim()]?.()
    burn()
  }

  set(options.scenario)

  // enforcement.md's decision table, as far as the UI can meet it.
  // Asked beside `refuse`: whether the plan includes a session of `size`.
  function refuseSize(size: string | undefined): MockResponse | undefined {
    if (!on || !b || b.mode !== "enforce" || b.state === "exempt") return undefined
    if (!size || !b.limits.sizes || b.limits.sizes.includes(size)) return undefined
    return refusal(403, "size_not_included", "Your plan does not include sessions of this size. Pick a smaller size, or change plan.")
  }

  function refuse(start: "create" | "resume"): MockResponse | undefined {
    if (!on || b.mode !== "enforce" || b.state === "exempt") return undefined
    if (b.state === "blocked") return refusal(403, "account_blocked", "This account is suspended. Contact support.")
    if (b.state === "terms") return refusal(403, "terms_required", "Accept the terms to continue.")
    if (b.state === "no_card") return refusal(402, "payment_method_required", "Add a payment method to create or wake sessions.")
    if (b.ledger !== "ok") {
      if (b.balanceMicros > 0) return undefined
      return { ...refusal(503, "metering_unavailable", "Billing is unavailable right now. Try again in a few minutes."), headers: { "Retry-After": "120" } }
    }
    if (b.balanceMicros <= 0) return refusal(402, "out_of_credit", "You are out of credit. Add credit or change plan to continue.")
    const n = b.limits
    if (start === "create" && sessions.size >= n.maxSessions)
      return refusal(409, "session_limit", `Your plan allows ${n.maxSessions} sessions. Delete one, or change plan.`, { limit: n.maxSessions })
    if (awake().length >= n.maxAwake)
      return refusal(409, "awake_limit", `Your plan runs ${n.maxAwake} sessions at once. Stop one, or change plan.`, { limit: n.maxAwake })
    return undefined
  }

  function usage(period: string | null): MockResponse {
    const closed = [iso(-42 * DAY), iso(-72 * DAY)]
    const current = !period
    if (period && !closed.includes(period)) return json(404, { error: "no such period" })
    const start = current ? b.period.start : period!
    const length = current ? 12 : 30
    const days = Array.from({ length }, (_, i) => {
      // Uneven, and the same on every read.
      const awakeSeconds = current ? ((i * 7919) % 5) * 3600 + ((i * 31) % 4) * 900 : ((i * 104729) % 4) * 2700
      return {
        date: new Date(new Date(start).getTime() + i * DAY).toISOString().slice(0, 10),
        awakeSeconds,
        awakeMicros: Math.round((awakeSeconds / 3600) * RATES.awakeMicrosPerHour),
        diskMicros: Math.max(sessions.size, 1) * RATES.sessionDiskGB * RATES.diskMicrosPerGBHour * 24,
      }
    })
    const awakeSeconds = days.reduce((n, d) => n + d.awakeSeconds, 0)
    const diskMicros = days.reduce((n, d) => n + d.diskMicros, 0)
    const list = [...sessions.values()]
    const shares = list.map((_, i) => 1 / (i + 1))
    const total = shares.reduce((a, x) => a + x, 0) || 1
    const lines = list.map((s, i) => {
      const seconds = Math.round((awakeSeconds * shares[i]) / total)
      return {
        id: s.id,
        name: s.name,
        awakeSeconds: seconds,
        awakeMicros: Math.round((seconds / 3600) * RATES.awakeMicrosPerHour),
        diskMicros: Math.round(diskMicros / Math.max(list.length, 1)),
      }
    })
    // A session deleted during the period has no name.
    if (!current) lines.push({ id: "s-gone1", name: undefined as never, awakeSeconds: 5400, awakeMicros: 300_000, diskMicros: 120_000 })
    return json(200, {
      start,
      end: current ? b.period.end : new Date(new Date(start).getTime() + 30 * DAY).toISOString(),
      plan: b.plan.key,
      awakeSeconds,
      awakeMicros: days.reduce((n, d) => n + d.awakeMicros, 0),
      diskMicros,
      days,
      sessions: lines,
      periods: closed,
    })
  }

  function add(source: string, micros: number, days: number) {
    const others = b.balances.filter((x: any) => x.source !== source)
    const had = b.balances.find((x: any) => x.source === source)?.micros ?? 0
    const order = ["plan", "signup", "purchase", "admin"]
    setBalance([...others, { source, micros: had + micros, expiresAt: iso(days * DAY) }].sort((x, y) => order.indexOf(x.source) - order.indexOf(y.source)))
    if (b.balanceMicros > 0) {
      b.level = "ok"
      delete b.exhaustedAt
      delete b.sleepAt
      delete b.deleteAt
      for (const s of sessions.values()) {
        delete s.draining
        delete s.deleteAfter
      }
    }
  }

  function complete(c: { kind: string; item?: any; signup?: any }) {
    b.hasCustomer = true
    if (c.kind === "setup") {
      b.hasPaymentMethod = true
      b.card = { brand: "visa", last4: "4242", expMonth: 8, expYear: 2028 }
      if (b.state === "no_card") b.state = "active"
      const first = b.signupCredit?.state !== "granted"
      c.signup = !first
        ? b.signupCredit
        : signupOutcome === "granted"
          ? { state: "granted", amountMicros: SIGNUP.amountMicros }
          : { state: "refused", reason: signupOutcome }
      if (first) {
        b.signupCredit = c.signup
        if (c.signup.state === "granted") settle.push(() => add("signup", SIGNUP.amountMicros, SIGNUP.validDays))
      }
    } else if (c.kind === "plan") {
      settle.push(() => {
        subscribe(c.item)
        add("plan", c.item.creditMicros, 30)
      })
    } else {
      settle.push(() => add("purchase", c.item.creditMicros, c.item.validDays))
    }
  }

  // Answers the billing routes; undefined for anything that is not one (and
  // for everything while billing is off, which the caller turns into a 404).
  function handle(method: string, parts: string[], query: string, body: any): MockResponse | undefined {
    if (!on) return undefined
    const route = `${method} /${parts.slice(1).join("/")}`

    if (route === "GET /billing") {
      for (const fn of settle.splice(0)) fn()
      burn()
      return json(200, b)
    }
    if (route === "GET /billing/catalogue") return json(200, CATALOGUE)
    if (route === "GET /billing/usage") return usage(new URLSearchParams(query).get("period"))

    if (route === "POST /billing/checkout") {
      if (b.state === "blocked") return refusal(403, "account_blocked", "This account is suspended. Contact support.")
      if (b.payments === "off") return refusal(503, "payments_off", "Payments are not set up.")
      if (stripeDown) return refusal(502, "stripe_unavailable", "The payment service did not answer. Nothing was charged.")
      const plan = PLANS.find(p => p.lookupKey === body.item)
      const pack = PACKS.find(p => p.lookupKey === body.item)
      if (body.item !== undefined && !plan && !pack) return refusal(400, "unknown_item", "That is not for sale.")
      if (plan && b.subscription) return refusal(409, "already_subscribed", "You already have a plan. Change it under Manage billing.")
      const id = `cs_test_mock${++counter}`
      checkouts.set(id, { kind: plan ? "plan" : pack ? "purchase" : "setup", item: plan ?? pack, polls: checkoutPolls, done: false })
      // No Stripe here: the "payment page" is the address Stripe would send the browser back to.
      return json(200, { url: `/billing?checkout=${id}` })
    }
    if (method === "GET" && parts[1] === "billing" && parts[2] === "checkout" && parts[3]) {
      const c = checkouts.get(parts[3])
      if (!c) return json(404, { error: "not a checkout of this account" })
      if (!c.done && c.polls-- > 0) return json(200, { status: "open", kind: c.kind })
      if (!c.done) {
        c.done = true
        complete(c)
      }
      return json(200, {
        status: "complete",
        kind: c.kind,
        ...(c.item ? { item: c.item.lookupKey } : {}),
        ...(c.kind === "setup" ? { signupCredit: c.signup } : {}),
      })
    }
    if (route === "POST /billing/subscription") {
      const plan = PLANS.find(p => p.lookupKey === body.item)
      if (!plan) return refusal(400, "unknown_item", "That plan is not available.")
      if (!b.subscription) return refusal(409, "not_subscribed", "You have no subscription to change. Subscribe to a plan instead.")
      if (stripeDown) return refusal(502, "stripe_unavailable", "The payment service did not answer.")
      if (plan.key === b.plan.key) return json(200, { change: "kept", item: plan.lookupKey })
      if (plan.amount > (b.plan.amount ?? 0)) {
        subscribe(plan)
        return json(200, { change: "upgraded", item: plan.lookupKey, effectiveAt: new Date().toISOString() })
      }
      return json(200, { change: "scheduled", item: plan.lookupKey, effectiveAt: b.period.end })
    }
    if (route === "POST /billing/portal") {
      if (!b.hasCustomer) return refusal(409, "no_customer", "There is nothing to manage yet.")
      if (stripeDown) return refusal(502, "stripe_unavailable", "The payment service did not answer.")
      return json(200, { url: "/billing?portal=mock" })
    }
    if (route === "PUT /billing/auto-recharge") {
      if (!b.autoRecharge?.available) return undefined
      if (!body.enabled) {
        b.autoRecharge = { ...b.autoRecharge, enabled: false }
        return json(200, b.autoRecharge)
      }
      if (!b.hasPaymentMethod) return refusal(402, "payment_method_required", "Add a payment method first.")
      if (body.agree !== true) return json(400, { error: "the agreement must be accepted" })
      if (!PACKS.some(p => p.key === body.pack)) return refusal(400, "unknown_item", "That is not for sale.")
      if (!(body.monthlyCapCents > 0) || body.monthlyCapCents > 50_000) return json(400, { error: "the monthly cap is at most $500" })
      const { disabledReason: _cleared, ...rest } = b.autoRecharge
      b.autoRecharge = { ...rest, enabled: true, pack: body.pack, thresholdMicros: body.thresholdMicros, monthlyCapCents: body.monthlyCapCents }
      return json(200, b.autoRecharge)
    }
    if (route === "POST /billing/terms") {
      if (body.version !== b.termsRequired) return json(409, { error: "not the current version of the terms" })
      delete b.termsRequired
      b.state = b.hasPaymentMethod ? "active" : "no_card"
      return { status: 204 }
    }
    if (route === "DELETE /account") {
      if (body.confirm !== email) return json(400, { error: "that is not your email address" })
      if (stripeDown) return refusal(502, "stripe_unavailable", "The payment service did not answer. Nothing was deleted.")
      sessions.clear()
      b.state = "blocked"
      return { status: 204 }
    }
    return undefined
  }

  // The session view's billing fields.
  const view = (s: BillingSession) =>
    on
      ? {
          ...(s.stoppedBy && s.state !== "running" && s.state !== "starting" ? { stoppedBy: s.stoppedBy } : {}),
          ...(s.draining ? { draining: s.draining } : {}),
          ...(s.deleteAfter ? { deleteAfter: s.deleteAfter } : {}),
        }
      : {}

  return { handle, refuse, refuseSize, view, set, isOn: () => on, state: () => b }
}

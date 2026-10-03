// The client of docs/contracts/billing/backend-api.yaml, and what the UI works
// out from its answers. None of the routes exists while the backend has
// billing off: `get` then resolves to null and nothing of billing is shown.
//
// Every price, rate and limit comes from the API (the catalogue is data);
// nothing here knows a number.
import type { Session } from "./api"
import { ApiError, SignedOutError, withRefusal } from "./api"

export type Micros = number // micro-dollars: 1 USD = 1000000

export interface Rates {
  awakeMicrosPerHour: Micros
  diskMicrosPerGBHour: Micros
  sessionDiskGB: number
}

export interface SignupCredit {
  state: "pending" | "granted" | "refused"
  reason?: "card-used" | "prepaid" | "wallet" | "no-fingerprint"
  amountMicros?: Micros
}

export interface AutoRecharge {
  available: boolean
  enabled: boolean
  pack?: string
  thresholdMicros?: Micros
  monthlyCapCents?: number
  chargedCents?: number
  lastStatus?: "pending" | "succeeded" | "failed"
  disabledReason?: "payment-failed" | "authentication-required" | "no-card" | "cap-reached"
}

export type BalanceSource = "plan" | "signup" | "purchase" | "admin"

export interface Billing {
  mode: "meter" | "enforce"
  state: "blocked" | "exempt" | "terms" | "no_card" | "active"
  ledger: "ok" | "pending" | "stale"
  hasPaymentMethod?: boolean
  card?: { brand?: string; last4?: string; expMonth?: number; expYear?: number }
  signupCredit?: SignupCredit
  plan: { key: string; name: string; amount?: number; creditMicros?: Micros }
  subscription?: {
    status: "incomplete" | "incomplete_expired" | "trialing" | "active" | "past_due" | "canceled" | "unpaid" | "paused"
    renewsAt?: string
    cancelsAt?: string
  }
  level: "ok" | "low" | "exhausted"
  balanceMicros: Micros
  balances?: { source: BalanceSource; micros: Micros; expiresAt?: string }[]
  burnMicrosPerHour?: Micros
  rates: Rates
  exhaustedAt?: string
  sleepAt?: string
  deleteAt?: string
  period: {
    start: string
    end: string
    planCreditMicros?: Micros
    awakeSeconds: number
    awakeMicros: Micros
    diskMicros: Micros
  }
  // The awake rate of each size of session, small first; and the sizes the
  // plan includes. Absent from a backend without sizes.
  sizes?: SizeRate[]
  limits: { maxSessions: number; maxAwake: number; sizes?: string[] }
  autoRecharge?: AutoRecharge
  payments: "off" | "test" | "live"
  hasCustomer?: boolean
  termsRequired?: string
  // Not in the contract's schema: when the ledger's numbers were last written,
  // shown with a stale ledger if the backend sends it.
  observedAt?: string
}

export interface UsageLine {
  awakeSeconds?: number
  awakeMicros?: Micros
  diskMicros?: Micros
}

export interface Usage {
  start: string
  end: string
  plan?: string
  awakeSeconds: number
  awakeMicros: Micros
  diskMicros: Micros
  days: ({ date: string } & UsageLine)[]
  sessions: ({ id: string; name?: string } & UsageLine)[]
  periods: string[]
}

export interface Item {
  key: string
  name: string
  lookupKey: string
  amount: number // US cents
  creditMicros: Micros
  maxSessions?: number
  maxAwake?: number
  sizes?: string[] // of a plan: the sizes of session it includes
  validDays?: number
}

export interface SizeRate {
  key: string
  awakeMicrosPerHour: Micros
}

export interface Catalogue {
  currency: string
  rates: Rates
  sizes?: SizeRate[]
  signupCredit: { amountMicros?: Micros; validDays?: number }
  payg: { maxSessions?: number; maxAwake?: number; sizes?: string[] }
  plans: Item[]
  packs: Item[]
}

export interface CheckoutState {
  status: "open" | "complete" | "expired"
  kind?: "setup" | "plan" | "purchase"
  signupCredit?: SignupCredit
  item?: string
}

export interface AutoRechargeInput {
  enabled: boolean
  pack?: string
  thresholdMicros?: Micros
  monthlyCapCents?: number
  agree?: boolean
}

const CHECKOUT_ID = /^cs_[A-Za-z0-9_]+$/
export const isCheckoutId = (id: string) => CHECKOUT_ID.test(id)

const isJson = (res: Response) => /^application\/([\w.-]+\+)?json\b/i.test(res.headers.get("Content-Type") ?? "")

type Fetch = typeof fetch

export function createBillingApi(fetchImpl: Fetch = (input, init) => fetch(input, init)) {
  // As api.ts: the proxy's cookie authenticates, and a lost proxy session is a
  // 401, an opaque redirect or the sign-in page's HTML.
  async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
    const headers = new Headers({ Accept: "application/json" })
    if (body !== undefined) headers.set("Content-Type", "application/json")
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

  return {
    // null where the backend has billing off (the route does not exist).
    get: async (): Promise<Billing | null> => {
      try {
        return await call<Billing>("GET", "/api/billing")
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) return null
        throw e
      }
    },
    usage: (period?: string) =>
      call<Usage>("GET", period ? `/api/billing/usage?period=${encodeURIComponent(period)}` : "/api/billing/usage"),
    catalogue: () => call<Catalogue>("GET", "/api/billing/catalogue"),
    // Without an item: a Checkout that saves a card and charges nothing.
    checkout: (item?: string) => call<{ url: string }>("POST", "/api/billing/checkout", item ? { item } : {}),
    checkoutState: async (id: string) => {
      if (!isCheckoutId(id)) throw new ApiError(404, "checkout not found")
      return call<CheckoutState>("GET", `/api/billing/checkout/${encodeURIComponent(id)}`)
    },
    portal: () => call<{ url: string }>("POST", "/api/billing/portal"),
    // For a subscriber: a dearer plan now, a cheaper one at the period's end.
    changePlan: (item: string) => call<PlanChange>("POST", "/api/billing/subscription", { item }),
    setAutoRecharge: (input: AutoRechargeInput) => call<AutoRecharge>("PUT", "/api/billing/auto-recharge", input),
    acceptTerms: (version: string) => call<void>("POST", "/api/billing/terms", { version }),
    deleteAccount: (confirm: string) => call<void>("DELETE", "/api/account", { confirm }),
  }
}

export type BillingApi = ReturnType<typeof createBillingApi>

export const billingApi = createBillingApi()

// Leaving for Stripe's pages (and coming back from the mock's). Behind an
// object so that a test can watch it: jsdom does not navigate.
export const browser = {
  go(url: string) {
    window.location.assign(url)
  },
}

// ---- Money and time, as shown --------------------------------------------

// Dollars and cents, rounded down: nobody is shown credit they do not have.
export function dollars(micros: Micros): string {
  const cents = Math.floor(Math.max(micros, 0) / 10_000)
  return `$${Math.floor(cents / 100)}.${String(cents % 100).padStart(2, "0")}`
}

// A price or a pack: "$5" when it is whole dollars, "$5.50" otherwise.
export function price(cents: number): string {
  return cents % 100 === 0 ? `$${cents / 100}` : `$${Math.floor(cents / 100)}.${String(cents % 100).padStart(2, "0")}`
}

export const credit = (micros: Micros) => price(Math.floor(micros / 10_000))

const HOURS_PER_MONTH = 730 // the catalogue's month

// What keeping one session costs a month: its disk, awake or asleep.
export const diskMicrosPerMonth = (rates: Rates) => rates.diskMicrosPerGBHour * rates.sessionDiskGB * HOURS_PER_MONTH

// Whole hours a balance lasts at a rate of use; null when nothing is using it.
export function hoursLeft(balanceMicros: Micros, burnMicrosPerHour: Micros | undefined): number | null {
  if (!burnMicrosPerHour || burnMicrosPerHour <= 0) return null
  return Math.floor(Math.max(balanceMicros, 0) / burnMicrosPerHour)
}

// Awake hours an amount of credit buys once the kept sessions' disks are
// paid for the month. An estimate, and always labelled as one.
export function awakeHours(creditMicros: Micros, rates: Rates, keptSessions = 0): number {
  if (rates.awakeMicrosPerHour <= 0) return 0
  return Math.max(Math.floor((creditMicros - keptSessions * diskMicrosPerMonth(rates)) / rates.awakeMicrosPerHour), 0)
}

export function aboutHours(hours: number): string {
  if (hours < 1) return "less than an hour"
  return `about ${hours} ${hours === 1 ? "hour" : "hours"}`
}

export function duration(seconds: number): string {
  const minutes = Math.floor(seconds / 60)
  return `${Math.floor(minutes / 60)} h ${String(minutes % 60).padStart(2, "0")}`
}

const fmt = (iso: string, options: Intl.DateTimeFormatOptions) => new Date(iso).toLocaleDateString("en-GB", options)
export const shortDate = (iso: string) => fmt(iso, { day: "numeric", month: "short" }) // 5 Nov
export const longDate = (iso: string) => fmt(iso, { day: "numeric", month: "long", year: "numeric" }) // 5 November 2026
export const dayMonth = (iso: string) => fmt(iso, { day: "numeric", month: "long" }) // 16 October
export const clock = (iso: string) => new Date(iso).toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" })

// "$0.20 for each hour a session is awake, and $1.40 a month for each session you keep"
export function ratesInWords(rates: Rates): { awake: string; kept: string } {
  return { awake: dollars(rates.awakeMicrosPerHour), kept: dollars(diskMicrosPerMonth(rates)) }
}

// The awake rate of a session of `size`: its own, or small's where the
// backend names none for it.
export function awakeRate(billing: Pick<Billing, "rates" | "sizes">, size?: string): Micros {
  return billing.sizes?.find(s => s.key === size)?.awakeMicrosPerHour ?? billing.rates.awakeMicrosPerHour
}

// Whether the account's plan includes sessions of `size`. A backend that
// does not say includes them all: it is the one that refuses.
export function planIncludes(billing: Pick<Billing, "limits">, size: string): boolean {
  return !billing.limits.sizes || billing.limits.sizes.includes(size)
}

export const SOURCE_LABEL: Record<BalanceSource, string> = {
  plan: "Plan credit",
  signup: "Sign-up credit",
  purchase: "Purchased credit",
  admin: "Credit from support",
}

// ---- What the account may do, worked out before asking ----------------------

// Whether anything is refused or stopped for this account. In shadow mode
// (`meter`) and for an exempt account nothing is: usage is only shown.
export const enforced = (b: Billing) => b.mode === "enforce" && b.state !== "exempt"

const isAwake = (s: Pick<Session, "state">) => s.state === "running" || s.state === "starting"

// What is drawing on the credit now.
export function drawing(b: Billing, sessions: Pick<Session, "state">[]) {
  const awake = sessions.filter(isAwake).length
  return { awake, kept: sessions.length, perHour: b.burnMicrosPerHour ?? 0 }
}

// The first-run gate: shown in place of every page. A user whose card was
// removed and who still has sessions keeps the app, to see, stop and delete them.
export function gateStep(b: Billing, sessionCount: number): "terms" | "card" | null {
  if (b.mode !== "enforce") return null
  if (b.state === "terms") return "terms"
  if (b.state === "no_card" && sessionCount === 0) return "card"
  return null
}

export type RefusalCode =
  | "payment_method_required"
  | "out_of_credit"
  | "session_limit"
  | "awake_limit"
  | "at_capacity"
  | "rate_limited"
  | "metering_unavailable"
  | "account_blocked"
  | "terms_required"
  | "size_not_included"

export interface Refusal {
  code: RefusalCode
  limit?: number
  message?: string // the server's sentence, when it was the one to refuse
}

// Why a create would be refused, from the account and the list, in the order
// of enforcement.md's decision table. null: nothing known against it (the
// server still decides).
export function createRefusal(b: Billing, sessions: Pick<Session, "state">[]): Refusal | null {
  if (!enforced(b)) return null
  if (b.state === "blocked") return { code: "account_blocked" }
  if (b.state === "terms") return { code: "terms_required" }
  if (b.state === "no_card") return { code: "payment_method_required" }
  if (b.ledger !== "ok") return null // rows 6 and 7 are the server's to answer
  if (b.level === "exhausted" || b.balanceMicros <= 0) return { code: "out_of_credit" }
  if (sessions.length >= b.limits.maxSessions) return { code: "session_limit", limit: b.limits.maxSessions }
  if (sessions.filter(isAwake).length >= b.limits.maxAwake) return { code: "awake_limit", limit: b.limits.maxAwake }
  return null
}

const REFUSALS = new Set([
  "size_not_included",
  "payment_method_required",
  "out_of_credit",
  "session_limit",
  "awake_limit",
  "at_capacity",
  "rate_limited",
  "metering_unavailable",
  "account_blocked",
  "terms_required",
])

// The server's refusal of a create or a wake, if that is what the error is.
export function refusalOf(e: unknown): Refusal | null {
  if (!(e instanceof ApiError) || !e.code || !REFUSALS.has(e.code)) return null
  return { code: e.code as RefusalCode, limit: e.limit, message: e.message }
}

export type WakeBlock = "credit" | "payment-method" | "blocked"

// Why a sleeping session cannot wake. Once there is credit and a card again
// it behaves as any sleeping session, whatever put it to sleep. Without the
// account (the page has no billing context) the session's own reason stands.
export function wakeBlock(s: Pick<Session, "state" | "stoppedBy">, b?: Billing | null): WakeBlock | null {
  if (s.state === "running" || s.state === "starting") return null
  const by = s.stoppedBy
  if (by !== "credit" && by !== "payment-method" && by !== "blocked") return null
  if (!b) return by
  if (!enforced(b)) return null
  if (b.state === "blocked") return "blocked"
  if (by === "blocked") return null
  if (b.state === "no_card") return "payment-method"
  if (b.ledger === "ok" && (b.level === "exhausted" || b.balanceMicros <= 0)) return "credit"
  return null
}

export const WAKE_BLOCK_LABEL: Record<WakeBlock, string> = {
  credit: "Asleep: out of credit",
  "payment-method": "Asleep: no payment method",
  blocked: "Suspended",
}

// ---- Banners -------------------------------------------------------------

export type BillingAction = "add-credit" | "plans" | "add-card" | "portal" | "create"

// What POST /api/billing/subscription did.
export interface PlanChange {
  change: "upgraded" | "scheduled" | "kept" | "none"
  item: string
  effectiveAt?: string
}

// What to tell the user about a plan change.
export function planChangeMessage(c: PlanChange, name: string): string {
  if (c.change === "upgraded") return `You are on ${name} now. Its credit for the new period has been added.`
  if (c.change === "scheduled")
    return `Your plan changes to ${name}${c.effectiveAt ? ` on ${dayMonth(c.effectiveAt)}` : " at the end of this period"}. Until then it is as it is.`
  return `Your plan stays ${name}.`
}

export interface Banner {
  id: string
  type: "success" | "info" | "warning" | "error" | "in-progress"
  text: string
  actions: { action: BillingAction; label: string }[]
  dismissible: boolean
}

const ADD_CREDIT = { action: "add-credit", label: "Add credit" } as const
const SEE_PLANS = { action: "plans", label: "See plans" } as const
const ADD_CARD = { action: "add-card", label: "Add a card" } as const

const DAY = 86_400_000

const RECHARGE_OFF: Record<NonNullable<AutoRecharge["disabledReason"]>, (cap: string) => string> = {
  "payment-failed": () => "Your card was declined, so auto-recharge is off.",
  "authentication-required": () =>
    "Your bank asked for confirmation, so auto-recharge is off. Add credit now to confirm with your bank.",
  "cap-reached": cap => `Auto-recharge reached your monthly cap${cap ? ` of ${cap}` : ""}.`,
  "no-card": () => "You have no saved card, so auto-recharge is off.",
}

// The banners of ui-states.md for an account, most serious first. None in
// shadow mode or for an exempt account.
export function banners(b: Billing, sessions: Pick<Session, "state">[], now: Date = new Date()): Banner[] {
  if (!enforced(b) || b.state === "blocked") return []
  if (gateStep(b, sessions.length)) return [] // the gate says what is needed
  const out: Banner[] = []
  const deleteIn = b.deleteAt ? new Date(b.deleteAt).getTime() - now.getTime() : undefined
  const exhausted = b.level === "exhausted"

  if (b.state === "no_card") {
    let text = "You have no payment method. Your sessions are asleep and kept. Add a card to wake them or create new ones."
    if (exhausted && b.deleteAt) text += ` They will be deleted on ${dayMonth(b.deleteAt)} unless you add a card and credit.`
    out.push({ id: "no-card", type: "error", text, actions: [ADD_CARD], dismissible: false })
  } else if (exhausted) {
    const grace = b.sleepAt && new Date(b.sleepAt).getTime() > now.getTime() && sessions.some(isAwake)
    if (grace) {
      out.push({
        id: "grace",
        type: "error",
        text: `You are out of credit. Running sessions go to sleep at ${clock(b.sleepAt!)}; work in progress finishes first and nothing is lost.`,
        actions: [ADD_CREDIT],
        dismissible: false,
      })
    } else {
      let text = "You are out of credit. Your sessions are asleep and kept."
      if (b.subscription && !b.subscription.cancelsAt) text += ` Your plan's credit returns on ${dayMonth(b.period.end)}.`
      if (deleteIn !== undefined) {
        text +=
          deleteIn <= DAY
            ? " Your sessions will be deleted tomorrow."
            : ` They will be deleted on ${dayMonth(b.deleteAt!)} unless you add credit.`
      }
      out.push({
        id: "exhausted",
        type: "error",
        text,
        actions: [ADD_CREDIT, SEE_PLANS],
        dismissible: deleteIn === undefined || deleteIn > 7 * DAY,
      })
    }
  } else if (b.level === "low" && !b.autoRecharge?.enabled) {
    const hours = hoursLeft(b.balanceMicros, b.burnMicrosPerHour)
    out.push({
      id: "low",
      type: "warning",
      text:
        hours === null
          ? `${dollars(b.balanceMicros)} of credit left.`
          : `${dollars(b.balanceMicros)} of credit left, ${aboutHours(hours)} at your current use.`,
      actions: [ADD_CREDIT, SEE_PLANS],
      dismissible: true,
    })
  }

  if (b.subscription?.status === "past_due" || b.subscription?.status === "unpaid") {
    out.push({
      id: "payment-failed",
      type: "error",
      text: "Your last payment failed. Update your card to keep your plan.",
      actions: [{ action: "portal", label: "Update card" }],
      dismissible: false,
    })
  }

  const reason = b.autoRecharge?.disabledReason
  if (reason && RECHARGE_OFF[reason]) {
    const cap = b.autoRecharge?.monthlyCapCents
    out.push({
      id: "recharge-off",
      type: "error",
      text: RECHARGE_OFF[reason](cap === undefined ? "" : price(cap)),
      actions: reason === "no-card" ? [ADD_CARD] : [ADD_CREDIT],
      dismissible: false,
    })
  }
  return out
}

// ---- Coming back from Checkout -------------------------------------------

export type CheckoutOutcome =
  | { result: "done"; state: CheckoutState; billing: Billing }
  | { result: "late"; state?: CheckoutState } // paid or saved, not yet in the ledger
  | { result: "expired" }
  | { result: "unknown" } // not a Checkout of this account

interface FollowOptions {
  before?: Micros // the balance when the Checkout was started
  sleep?: (ms: number) => Promise<void>
  cancelled?: () => boolean
}

export const CHECKOUT_POLL_MS = 2000
export const CHECKOUT_WAIT_MS = 30_000
export const BALANCE_WAIT_MS = 90_000

// Follows a Checkout the browser has come back from: asks what became of it
// every 2 s for up to 30 s, then reads the account until the credit (or the
// card) has arrived, for up to 90 s more.
export async function followCheckout(
  apiImpl: Pick<BillingApi, "checkoutState" | "get">,
  id: string,
  { before, sleep = ms => new Promise(r => setTimeout(r, ms)), cancelled = () => false }: FollowOptions = {},
): Promise<CheckoutOutcome> {
  let state: CheckoutState | undefined
  for (let waited = 0; ; waited += CHECKOUT_POLL_MS) {
    try {
      state = await apiImpl.checkoutState(id)
    } catch (e) {
      if (e instanceof SignedOutError) throw e
      if (e instanceof ApiError && e.status === 404) return { result: "unknown" }
      // Anything else is worth asking again.
    }
    if (state?.status === "expired") return { result: "expired" }
    if (state?.status === "complete") break
    if (waited >= CHECKOUT_WAIT_MS || cancelled()) return { result: "late", state }
    await sleep(CHECKOUT_POLL_MS)
  }

  const arrived = (b: Billing) => {
    if (state!.kind === "setup") {
      if (!b.hasPaymentMethod) return false
      // No credit is coming for a refused card: the card is all there is to wait for.
      if (state!.signupCredit?.state !== "granted") return true
    }
    return before === undefined ? b.balanceMicros > 0 : b.balanceMicros > before
  }
  for (let waited = 0; ; waited += CHECKOUT_POLL_MS) {
    try {
      const b = await apiImpl.get()
      if (b && arrived(b)) return { result: "done", state, billing: b }
    } catch (e) {
      if (e instanceof SignedOutError) throw e
    }
    if (waited >= BALANCE_WAIT_MS || cancelled()) return { result: "late", state }
    await sleep(CHECKOUT_POLL_MS)
  }
}

const SIGNUP_REFUSED: Record<string, string> = {
  "card-used": " This card has already been used for a sign-up credit.",
  prepaid: " Prepaid cards do not get the sign-up credit.",
  wallet: " Cards added through Apple Pay or Google Pay do not get the sign-up credit.",
}

// What to say once a Checkout is done, and what to offer.
export function checkoutMessage(
  state: CheckoutState,
  billing: Billing,
  catalogue: Catalogue | null,
): { type: "success" | "info"; text: string; action?: "create" | "add-credit" } {
  if (state.kind === "setup") {
    const signup = state.signupCredit
    if (signup?.state === "granted") {
      const until = billing.balances?.find(x => x.source === "signup")?.expiresAt
      const amount = signup.amountMicros ?? catalogue?.signupCredit.amountMicros
      return {
        type: "success",
        text: `Card saved.${amount ? ` ${credit(amount)} of credit added${until ? `, valid until ${dayMonth(until)}` : ""}.` : ""}`,
        action: "create",
      }
    }
    if (signup?.state === "refused")
      return { type: "info", text: `Card saved.${SIGNUP_REFUSED[signup.reason ?? ""] ?? ""}`, action: "add-credit" }
    return { type: "success", text: "Card saved." }
  }
  const items = [...(catalogue?.plans ?? []), ...(catalogue?.packs ?? [])]
  const item = items.find(i => i.lookupKey === state.item || i.key === state.item)
  if (state.kind === "plan") {
    const added = item ? ` ${credit(item.creditMicros)} of credit added.` : ""
    return { type: "success", text: `You are on ${item?.name ?? billing.plan.name}.${added}` }
  }
  return { type: "success", text: item ? `${credit(item.creditMicros)} of credit added.` : "Credit added." }
}

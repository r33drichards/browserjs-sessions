import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { createMockBackend } from "../mock/backend"
import { ApiError, SignedOutError } from "./api"
import type { Billing, CheckoutState } from "./billingApi"
import {
  aboutHours,
  awakeHours,
  banners,
  checkoutMessage,
  createBillingApi,
  createRefusal,
  credit,
  diskMicrosPerMonth,
  dollars,
  duration,
  followCheckout,
  gateStep,
  hoursLeft,
  price,
  ratesInWords,
  refusalOf,
  wakeBlock,
} from "./billingApi"

const NOW = new Date("2026-10-07T12:00:00Z")

function backend(scenario: string, extra: { checkoutPolls?: number } = {}) {
  const mock = createMockBackend({ presets: [], seed: false, billing: scenario, now: () => NOW, ...extra })
  return { mock, client: createBillingApi(mock.fetch), account: () => mock.billing.state() as Billing }
}

const running = [{ state: "running" as const }]
const asleep = [{ state: "asleep" as const }]

describe("the client", () => {
  it("says billing is off, without an error, when the route is a 404", async () => {
    const { client } = backend("off")
    expect(await client.get()).toBeNull()
  })

  it("reads the account, the catalogue and the usage", async () => {
    const { client } = backend("active")
    const b = await client.get()
    expect(b?.state).toBe("active")
    expect(b?.plan.key).toBe("pro")
    const catalogue = await client.catalogue()
    expect(catalogue.packs.map(p => p.key)).toEqual(["credit-5", "credit-20", "credit-50"])
    const usage = await client.usage()
    expect(usage.days.length).toBeGreaterThan(0)
    expect((await client.usage(usage.periods[0])).start).toBe(usage.periods[0])
    await expect(client.usage("2001-01-01T00:00:00Z")).rejects.toMatchObject({ status: 404 })
  })

  it("starts a setup Checkout with no item, and a purchase with the pack's lookup key", async () => {
    const sent: { path: string; body: unknown }[] = []
    const { mock } = backend("active")
    const client = createBillingApi((input, init) => {
      sent.push({ path: String(input), body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined })
      return mock.fetch(input, init)
    })
    expect((await client.checkout()).url).toMatch(/^\/billing\?checkout=cs_test_/)
    await client.checkout("cu_credit_20_v1")
    expect(sent.map(s => s.body)).toEqual([{}, { item: "cu_credit_20_v1" }])
  })

  it("carries the contract's code on an error", async () => {
    const { client } = backend("active+stripe-down")
    const refused = await client.checkout().catch(e => e)
    expect(refused).toBeInstanceOf(ApiError)
    expect(refused).toMatchObject({ status: 502, code: "stripe_unavailable" })
    await expect(client.checkout("nonsense")).rejects.toMatchObject({ status: 502 })
    const subscribed = backend("active").client
    await expect(subscribed.checkout("cu_starter_monthly_v1")).rejects.toMatchObject({ status: 409, code: "already_subscribed" })
    await expect(subscribed.checkout("cu_nothing")).rejects.toMatchObject({ status: 400, code: "unknown_item" })
    await expect(backend("new").client.portal()).rejects.toMatchObject({ status: 409, code: "no_customer" })
  })

  it("never asks about a checkout id that is not one", async () => {
    const fetchImpl = vi.fn()
    const client = createBillingApi(fetchImpl as never)
    await expect(client.checkoutState("../../sessions")).rejects.toMatchObject({ status: 404 })
    expect(fetchImpl).not.toHaveBeenCalled()
  })

  it("treats a lost proxy session as signed out", async () => {
    const client = createBillingApi(async () => new Response("", { status: 401 }))
    await expect(client.get()).rejects.toBeInstanceOf(SignedOutError)
    const html = createBillingApi(async () => new Response("<html>", { status: 200, headers: { "Content-Type": "text/html" } }))
    await expect(html.get()).rejects.toBeInstanceOf(SignedOutError)
  })
})

describe("formatting", () => {
  it("shows dollars and cents rounded down", () => {
    expect(dollars(7_400_000)).toBe("$7.40")
    expect(dollars(7_409_999)).toBe("$7.40")
    expect(dollars(9_999)).toBe("$0.00")
    expect(dollars(1_999_999)).toBe("$1.99")
    expect(dollars(123_450_000)).toBe("$123.45")
    expect(dollars(-5)).toBe("$0.00")
  })

  it("shows a price without cents when it has none", () => {
    expect(price(500)).toBe("$5")
    expect(price(550)).toBe("$5.50")
    expect(credit(20_000_000)).toBe("$20")
  })

  it("estimates the hours a balance lasts, rounded down", () => {
    expect(hoursLeft(7_400_000, 200_000)).toBe(37)
    expect(hoursLeft(800_000, 259_000)).toBe(3)
    expect(hoursLeft(100_000, 200_000)).toBe(0)
    expect(hoursLeft(5_000_000, 0)).toBeNull()
    expect(hoursLeft(5_000_000, undefined)).toBeNull()
    expect(aboutHours(37)).toBe("about 37 hours")
    expect(aboutHours(1)).toBe("about 1 hour")
    expect(aboutHours(0)).toBe("less than an hour")
  })

  it("works the rates out of the catalogue's numbers", () => {
    const rates = { awakeMicrosPerHour: 200_000, diskMicrosPerGBHour: 384, sessionDiskGB: 5 }
    expect(diskMicrosPerMonth(rates)).toBe(1_401_600)
    expect(ratesInWords(rates)).toEqual({ awake: "$0.20", kept: "$1.40" })
    expect(awakeHours(5_000_000, rates)).toBe(25) // the sign-up credit
    expect(awakeHours(10_000_000, rates, 1)).toBe(42) // a plan's credit less one kept session
    expect(awakeHours(1_000_000, rates, 1)).toBe(0)
    // Other numbers, other words: nothing is fixed in the UI.
    expect(ratesInWords({ awakeMicrosPerHour: 350_000, diskMicrosPerGBHour: 500, sessionDiskGB: 10 })).toEqual({ awake: "$0.35", kept: "$3.65" })
  })

  it("shows awake time in hours and minutes", () => {
    expect(duration(43_500)).toBe("12 h 05")
    expect(duration(59)).toBe("0 h 00")
  })
})

describe("what the account may do", () => {
  it("gates a new user, and one who must accept the terms, but not one who still has sessions", () => {
    expect(gateStep(backend("new").account(), 0)).toBe("card")
    expect(gateStep(backend("terms").account(), 0)).toBe("terms")
    expect(gateStep(backend("no-card").account(), 2)).toBeNull()
    expect(gateStep(backend("active").account(), 0)).toBeNull()
    expect(gateStep(backend("meter").account(), 0)).toBeNull()
  })

  it("knows why a create would be refused", () => {
    expect(createRefusal(backend("active").account(), running)).toBeNull()
    expect(createRefusal(backend("no-card").account(), asleep)).toEqual({ code: "payment_method_required" })
    expect(createRefusal(backend("exhausted").account(), asleep)).toEqual({ code: "out_of_credit" })
    expect(createRefusal(backend("session-limit").account(), [...asleep, ...asleep, ...asleep])).toEqual({ code: "session_limit", limit: 3 })
    expect(createRefusal(backend("session-limit").account(), [...running, ...running])).toEqual({ code: "awake_limit", limit: 2 })
    // Nothing is refused in shadow mode, for an exempt account, or on a ledger the UI cannot judge.
    expect(createRefusal(backend("meter").account(), running)).toBeNull()
    expect(createRefusal(backend("exempt").account(), running)).toBeNull()
    expect(createRefusal(backend("exhausted+stale").account(), running)).toBeNull()
  })

  it("reads a refusal out of the server's error, and nothing out of any other", () => {
    const refused = Object.assign(new ApiError(409, "Your plan allows 3 sessions."), { code: "session_limit", limit: 3 })
    expect(refusalOf(refused)).toEqual({ code: "session_limit", limit: 3, message: "Your plan allows 3 sessions." })
    expect(refusalOf(new ApiError(409, "session limit reached"))).toBeNull()
    expect(refusalOf(new Error("x"))).toBeNull()
  })

  it("keeps a session asleep only while the reason still holds", () => {
    const byCredit = { state: "asleep" as const, stoppedBy: "credit" as const }
    const byCard = { state: "asleep" as const, stoppedBy: "payment-method" as const }
    expect(wakeBlock(byCredit, backend("exhausted").account())).toBe("credit")
    expect(wakeBlock(byCredit, backend("active").account())).toBeNull() // credit is back: it wakes as any other
    expect(wakeBlock(byCard, backend("no-card").account())).toBe("payment-method")
    expect(wakeBlock(byCard, backend("active").account())).toBeNull()
    expect(wakeBlock({ state: "asleep", stoppedBy: "idle" }, backend("exhausted").account())).toBeNull()
    expect(wakeBlock({ state: "running", stoppedBy: "credit" }, backend("exhausted").account())).toBeNull()
    expect(wakeBlock({ state: "asleep", stoppedBy: "blocked" }, backend("blocked").account())).toBe("blocked")
    // Without the account, the session's own reason stands.
    expect(wakeBlock(byCredit)).toBe("credit")
    expect(wakeBlock({ state: "asleep" })).toBeNull()
  })
})

describe("banners", () => {
  const of = (scenario: string, sessions: { state: "running" | "asleep" }[] = asleep) => banners(backend(scenario).account(), sessions, NOW)
  const labels = (b: { actions: { label: string }[] }) => b.actions.map(a => a.label)

  it("has none when all is well, in shadow mode, or for an exempt account", () => {
    expect(of("active")).toEqual([])
    expect(of("meter")).toEqual([])
    expect(of("exempt")).toEqual([])
    expect(of("ending")).toEqual([]) // said on the billing page only
    expect(of("new", [])).toEqual([]) // the gate says it
  })

  it("warns of a low balance, dismissibly, and not with auto-recharge on", () => {
    const account = { ...backend("low").account(), burnMicrosPerHour: 259_000 }
    const [low] = banners(account, running, NOW)
    expect(low).toMatchObject({ id: "low", type: "warning", dismissible: true })
    expect(low.text).toBe("$0.80 of credit left, about 3 hours at your current use.")
    expect(of("low")[0].text).toBe("$0.80 of credit left.") // nothing is using it
    expect(labels(low)).toEqual(["Add credit", "See plans"])
    expect(of("low+auto-on")).toEqual([])
  })

  it("says when running sessions go to sleep during the grace", () => {
    const [grace] = of("grace", running)
    expect(grace).toMatchObject({ id: "grace", type: "error", dismissible: false })
    expect(grace.text).toMatch(/^You are out of credit\. Running sessions go to sleep at \d\d:\d\d; work in progress finishes first and nothing is lost\.$/)
    expect(labels(grace)).toEqual(["Add credit"])
    // Nothing awake: the sessions are already asleep.
    expect(of("grace", asleep)[0].id).toBe("exhausted")
  })

  it("says the sessions are asleep and kept at zero, and when a plan's credit returns", () => {
    const [out] = of("exhausted")
    expect(out.text).toBe("You are out of credit. Your sessions are asleep and kept. Your plan's credit returns on 25 October.")
    expect(out).toMatchObject({ type: "error", dismissible: true })
    expect(labels(out)).toEqual(["Add credit", "See plans"])
  })

  it("names the day of deletion, and cannot be dismissed within a week of it", () => {
    const [soon] = of("deleting")
    expect(soon.text).toBe("You are out of credit. Your sessions are asleep and kept. They will be deleted on 12 October unless you add credit.")
    expect(soon.dismissible).toBe(false)
    const [tomorrow] = of("tomorrow")
    expect(tomorrow.text).toBe("You are out of credit. Your sessions are asleep and kept. Your sessions will be deleted tomorrow.")
    expect(tomorrow.dismissible).toBe(false)
  })

  it("asks for a card when it was removed", () => {
    const [none] = of("no-card")
    expect(none).toMatchObject({ id: "no-card", type: "error", dismissible: false })
    expect(none.text).toBe("You have no payment method. Your sessions are asleep and kept. Add a card to wake them or create new ones.")
    expect(labels(none)).toEqual(["Add a card"])
  })

  it("reports a failed payment and a failed auto-recharge", () => {
    const [failed] = of("past-due")
    expect(failed.text).toBe("Your last payment failed. Update your card to keep your plan.")
    expect(failed.actions).toEqual([{ action: "portal", label: "Update card" }])
    const [recharge] = of("recharge-failed")
    expect(recharge.text).toBe("Your bank asked for confirmation, so auto-recharge is off. Add credit now to confirm with your bank.")
    const account = backend("recharge-failed").account()
    account.autoRecharge!.disabledReason = "cap-reached"
    expect(banners(account, asleep, NOW)[0].text).toBe("Auto-recharge reached your monthly cap of $50.")
    account.autoRecharge!.disabledReason = "payment-failed"
    expect(banners(account, asleep, NOW)[0].text).toBe("Your card was declined, so auto-recharge is off.")
  })
})

describe("coming back from Checkout", () => {
  beforeEach(() => {
    vi.useFakeTimers()
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  // Starts the Checkout and follows it, running the clock until it settles.
  async function follow(scenario: string, item?: string, polls = 3) {
    const { mock, client, account } = backend(scenario, { checkoutPolls: polls })
    const before = account().balanceMicros
    const { url } = await client.checkout(item)
    const id = new URL(url, "http://x").searchParams.get("checkout")!
    const asked: string[] = []
    const watched = {
      checkoutState: (i: string) => (asked.push("checkout"), client.checkoutState(i)),
      get: () => (asked.push("billing"), client.get()),
    }
    const outcome = followCheckout(watched, id, { before })
    await vi.runAllTimersAsync()
    return { outcome: await outcome, asked, mock, catalogue: await client.catalogue() }
  }

  const said = (r: Awaited<ReturnType<typeof follow>>) => {
    if (r.outcome.result !== "done") throw new Error(`not done: ${r.outcome.result}`)
    return checkoutMessage(r.outcome.state, r.outcome.billing, r.catalogue)
  }

  it("asks every 2 s until the Checkout is complete, then reads the account until the credit is in", async () => {
    const started = Date.now()
    const r = await follow("new")
    expect(r.asked).toEqual(["checkout", "checkout", "checkout", "checkout", "billing"])
    expect(Date.now() - started).toBe(3 * 2000)
    expect(r.outcome.result).toBe("done")
  })

  it("setup, credit granted", async () => {
    const r = await follow("new")
    expect(said(r)).toEqual({ type: "success", text: "Card saved. $5 of credit added, valid until 5 January.", action: "create" })
  })

  it.each([
    ["card-used", "Card saved. This card has already been used for a sign-up credit."],
    ["prepaid", "Card saved. Prepaid cards do not get the sign-up credit."],
    ["wallet", "Card saved. Cards added through Apple Pay or Google Pay do not get the sign-up credit."],
  ])("setup, credit refused: %s", async (reason, text) => {
    const r = await follow(`new+${reason}`)
    expect(said(r)).toEqual({ type: "info", text, action: "add-credit" })
  })

  it("purchase", async () => {
    const r = await follow("payg", "cu_credit_20_v1")
    expect(said(r)).toEqual({ type: "success", text: "$20 of credit added." })
  })

  it("plan", async () => {
    const r = await follow("payg", "cu_starter_monthly_v1")
    expect(said(r)).toEqual({ type: "success", text: "You are on Starter. $10 of credit added." })
  })

  it("gives up on the Checkout after 30 s and says the credit will come", async () => {
    const started = Date.now()
    const r = await follow("payg", "cu_credit_5_v1", 1000)
    expect(r.outcome.result).toBe("late")
    expect(Date.now() - started).toBe(30_000)
    expect(r.asked.filter(a => a === "checkout")).toHaveLength(16)
    expect(r.asked).not.toContain("billing")
  })

  it("waits up to 90 s more for the balance to grow", async () => {
    const { client } = backend("payg")
    const complete: CheckoutState = { status: "complete", kind: "purchase", item: "cu_credit_5_v1" }
    const account = (await client.get())!
    const reads = vi.fn(async () => account) // the ledger never catches up
    const started = Date.now()
    const outcome = followCheckout({ checkoutState: async () => complete, get: reads }, "cs_test_1", { before: account.balanceMicros })
    await vi.runAllTimersAsync()
    expect(await outcome).toEqual({ result: "late", state: complete })
    expect(Date.now() - started).toBe(90_000)
    expect(reads).toHaveBeenCalledTimes(46)
  })

  it("says nothing of an abandoned Checkout, or of one that is not the caller's", async () => {
    const expired = followCheckout({ checkoutState: async () => ({ status: "expired" }), get: async () => null }, "cs_test_1")
    expect(await expired).toEqual({ result: "expired" })
    const { client } = backend("payg")
    expect(await followCheckout(client, "cs_test_unknown")).toEqual({ result: "unknown" })
  })
})

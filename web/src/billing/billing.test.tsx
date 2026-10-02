// @vitest-environment jsdom
// Every state of docs/contracts/billing/ui-states.md, against the mock of
// backend-api.yaml (mock/billing.ts), through the whole app.
import createWrapper from "@cloudscape-design/components/test-utils/dom"
import { cleanup, configure, fireEvent, screen, waitFor, within } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { App } from "../App"
import { browser } from "../billingApi"
import { SessionDetail } from "../pages/SessionDetail"
import { renderAt, startBackend } from "../test/harness"

// The viewer and the editor need a real browser.
vi.mock("../components/VncPane", () => ({ VncPane: () => <div data-testid="viewer" /> }))
vi.mock("../components/MonacoEditor", () => ({ default: () => null }))

// The whole app is loaded here, pages and all: the first render of each
// lazy page takes longer than the default second when every file runs at once.
configure({ asyncUtilTimeout: 5000 })

let went: string[]

beforeEach(() => {
  went = []
  vi.spyOn(browser, "go").mockImplementation(url => void went.push(url))
  localStorage.clear()
  sessionStorage.clear()
})

afterEach(() => {
  cleanup()
  vi.restoreAllMocks()
})

function open(path: string, scenario?: string, options: Parameters<typeof startBackend>[0] = {}) {
  const server = startBackend({ billing: scenario, ...options })
  const view = renderAt(path, [{ path: "*", element: <App /> }])
  return { ...server, ...view, at: () => view.router.state.location.pathname }
}

const banner = () => screen.findByTestId("billing-banners")
const button = (name: string | RegExp) => screen.getByRole("button", { name })
const billingCalls = (sent: { method: string; path: string }[]) => sent.filter(r => r.path.startsWith("/api/billing"))

describe("billing off", () => {
  it("is the app as it was: no balance, no banners, no page, and one question asked", async () => {
    const { sent, at } = open("/billing")
    await screen.findByRole("link", { name: "research" }) // /billing went to the list
    expect(at()).toBe("/")
    expect(screen.queryByRole("link", { name: "Billing" })).toBeNull()
    expect(screen.queryByTestId("billing-banners")).toBeNull()
    expect(document.body.textContent).not.toMatch(/credit|payment|\$/i)
    expect(billingCalls(sent).map(r => `${r.method} ${r.path}`)).toEqual(["GET /api/billing"])
  })

  it("leaves the create form and the session page alone", async () => {
    open("/sessions/create")
    await screen.findByRole("heading", { name: "Create session" })
    expect(screen.queryByTestId("create-cost")).toBeNull()
    expect(screen.queryByTestId("create-refusal")).toBeNull()
    expect(button("Create session").hasAttribute("disabled")).toBe(false)
  })

  it("treats a billing endpoint that fails as off", async () => {
    const server = startBackend()
    const real = globalThis.__testFetch!
    globalThis.__testFetch = async (input, init) =>
      String(input) === "/api/billing" ? new Response("boom", { status: 500 }) : real(input, init)
    renderAt("/", [{ path: "*", element: <App /> }])
    await screen.findByRole("link", { name: "research" })
    expect(screen.queryByRole("link", { name: "Billing" })).toBeNull()
    void server
  })
})

describe("the gate", () => {
  it("stands in place of every page for a new user with no card, who cannot reach the create form", async () => {
    const { sent } = open("/sessions/create", "new")
    expect(await screen.findByRole("heading", { name: "Welcome" })).toBeTruthy()
    expect(screen.queryByRole("heading", { name: "Create session" })).toBeNull()
    expect(screen.queryByRole("button", { name: "Create session" })).toBeNull()
    const text = document.body.textContent!
    expect(text).toContain("A card is required before you create a desktop. Nothing is charged now")
    expect(text).toContain("$5 of credit, enough for about 25 hours awake, valid for 90 days. One credit per person and per card.")
    expect(text).toContain("$0.20 for each hour a session is awake · $1.40 a month for each session you keep")
    expect(text).toContain("Test mode: no real money is taken.")
    expect(screen.queryByRole("heading", { name: /Terms/ })).toBeNull()

    fireEvent.click(button("Add a card"))
    await waitFor(() => expect(went).toHaveLength(1))
    expect(went[0]).toMatch(/^\/billing\?checkout=cs_test_/)
    expect(sent.find(r => r.method === "POST")).toMatchObject({ path: "/api/billing/checkout", body: {} })
  })

  it("is the same on the list and on the billing page", async () => {
    open("/", "new")
    expect(await screen.findByRole("heading", { name: "Welcome" })).toBeTruthy()
    expect(screen.queryByRole("table")).toBeNull()
    expect(screen.getByRole("link", { name: "Billing" }).textContent).toBe("Add a card")
  })

  it("asks for the terms first", async () => {
    const { sent } = open("/", "terms")
    expect(await screen.findByRole("heading", { name: "1. Terms" })).toBeTruthy()
    expect(screen.getByRole("link", { name: "terms of service" }).getAttribute("href")).toBe("https://computeruse.site/legal/terms")
    expect(button("Add a card").getAttribute("aria-disabled")).toBe("true")
    expect(button("Continue").hasAttribute("disabled")).toBe(true)
    fireEvent.click(screen.getByRole("checkbox"))
    fireEvent.click(button("Continue"))
    await waitFor(() => expect(screen.queryByRole("heading", { name: "1. Terms" })).toBeNull())
    expect(sent.find(r => r.path === "/api/billing/terms")?.body).toEqual({ version: "2026-10-01" })
    expect(button("Add a card").getAttribute("aria-disabled")).toBeNull()
  })

  it("lets a user whose card was removed keep the app, with the banner, to see and delete sessions", async () => {
    open("/", "no-card")
    const row = (await screen.findByRole("link", { name: "research" })).closest("tr")!
    expect((await banner()).textContent).toContain(
      "You have no payment method. Your sessions are asleep and kept. Add a card to wake them or create new ones.",
    )
    expect(within(await banner()).getByRole("button", { name: "Add a card" })).toBeTruthy()
    expect(within(await banner()).queryByRole("button", { name: "Dismiss" })).toBeNull()
    expect(row.textContent).toContain("Asleep: no payment method")
    expect(within(row).getByRole("button", { name: "Resume" }).getAttribute("aria-disabled")).toBe("true")
    expect(within(row).getByRole("button", { name: "Delete" }).hasAttribute("disabled")).toBe(false)
  })

  it("is one alert for a blocked account", async () => {
    open("/", "blocked")
    expect(await screen.findByText("This account is suspended")).toBeTruthy()
    expect(screen.getByRole("link", { name: "support@computeruse.site" })).toBeTruthy()
    expect(screen.queryByRole("table")).toBeNull()
  })
})

describe("banners and the balance", () => {
  it("shows the balance in the header and nothing else when all is well", async () => {
    open("/", "active")
    expect((await screen.findByRole("link", { name: "Billing" })).textContent).toBe("Billing $32.84")
    await screen.findByRole("link", { name: "research" })
    expect(screen.queryByTestId("billing-banners")).toBeNull()
  })

  it("low: a warning that is put away for the day", async () => {
    const first = open("/", "low")
    expect((await banner()).textContent).toMatch(/\$0\.80 of credit left, about \d+ hours? at your current use\./)
    expect(within(await banner()).getByRole("button", { name: "See plans" })).toBeTruthy()
    fireEvent.click(within(await banner()).getByRole("button", { name: "Dismiss" }))
    await waitFor(() => expect(screen.queryByTestId("billing-banners")).toBeNull())
    first.unmount()
    open("/", "low")
    await screen.findByRole("link", { name: "research" })
    expect(screen.queryByTestId("billing-banners")).toBeNull()
  })

  it("low: Add credit opens the packs, and Continue goes to Stripe with the chosen one", async () => {
    const { sent } = open("/", "low")
    fireEvent.click(within(await banner()).getByRole("button", { name: "Add credit" }))
    const modal = await screen.findByRole("dialog")
    expect(within(modal).getAllByRole("radio").map(r => r.closest("label")?.textContent ?? r.parentElement?.textContent)).toHaveLength(3)
    expect(modal.textContent).toContain("Credit is used after your plan's credit and is valid for 12 months.")
    expect(modal.textContent).toContain("Test mode: no real money is taken. Use card 4242 4242 4242 4242.")
    createWrapper(modal).findTiles()!.findItemByValue("cu_credit_50_v1")!.findNativeInput().click()
    fireEvent.click(within(modal).getByRole("button", { name: "Continue to payment" }))
    await waitFor(() => expect(went).toHaveLength(1))
    expect(sent.find(r => r.path === "/api/billing/checkout")?.body).toEqual({ item: "cu_credit_50_v1" })
  })

  it("grace: says when the running sessions sleep, and the screen says they are finishing", async () => {
    const { sessionNamed } = open("/", "grace")
    expect((await banner()).textContent).toMatch(/You are out of credit\. Running sessions go to sleep at \d\d:\d\d; work in progress finishes first/)
    expect(within(await banner()).queryByRole("button", { name: "Dismiss" })).toBeNull()
    expect(screen.getByRole("link", { name: "Billing" }).className).toContain("wf-balance-zero")
    cleanup()
    renderAt(`/sessions/${sessionNamed("research").id}`, [{ path: "*", element: <App /> }])
    expect(await screen.findByText("Finishing work in progress, then going to sleep.")).toBeTruthy()
    expect(screen.getByTestId("viewer")).toBeTruthy()
  })

  it("exhausted: asleep and kept, with the list saying why and Resume disabled", async () => {
    open("/", "exhausted")
    expect((await banner()).textContent).toMatch(/You are out of credit\. Your sessions are asleep and kept\. Your plan's credit returns on \d+ \w+\./)
    const row = (await screen.findByRole("link", { name: "research" })).closest("tr")!
    expect(row.textContent).toContain("Asleep: out of credit")
    expect(within(row).getByRole("button", { name: "Resume" }).getAttribute("aria-disabled")).toBe("true")
  })

  it("deletion: names the day, on the banner and on each session, and cannot be dismissed", async () => {
    open("/", "deleting")
    expect((await banner()).textContent).toMatch(/They will be deleted on \d+ \w+ unless you add credit\./)
    expect(within(await banner()).queryByRole("button", { name: "Dismiss" })).toBeNull()
    const row = (await screen.findByRole("link", { name: "research" })).closest("tr")!
    expect(row.textContent).toMatch(/Deleted on \d+ \w+ unless you add credit/)
    cleanup()
    open("/", "tomorrow")
    expect((await banner()).textContent).toContain("Your sessions will be deleted tomorrow.")
  })

  it("payment failed: Update card opens the portal", async () => {
    const { sent } = open("/", "past-due")
    expect((await banner()).textContent).toContain("Your last payment failed. Update your card to keep your plan.")
    fireEvent.click(within(await banner()).getByRole("button", { name: "Update card" }))
    await waitFor(() => expect(went).toEqual(["/billing?portal=mock"]))
    expect(sent.some(r => r.method === "POST" && r.path === "/api/billing/portal")).toBe(true)
  })

  it("auto-recharge failed", async () => {
    open("/", "recharge-failed")
    expect((await banner()).textContent).toContain("Your bank asked for confirmation, so auto-recharge is off. Add credit now to confirm with your bank.")
  })

  it("Stripe down: says nothing was charged", async () => {
    open("/", "no-card+stripe-down")
    fireEvent.click(within(await banner()).getByRole("button", { name: "Add a card" }))
    expect(await screen.findByText("The payment page could not be opened. Nothing was charged. Try again.")).toBeTruthy()
    expect(went).toEqual([])
  })

  it("shadow mode: no gate and no banners, and the page says nothing is limited", async () => {
    open("/billing", "meter")
    expect(await screen.findByText("Usage is shown for information. Nothing is limited yet.")).toBeTruthy()
    expect(screen.queryByTestId("billing-banners")).toBeNull()
    expect(screen.queryByRole("button", { name: "Add credit" })).toBeNull() // no Stripe in this stage
    cleanup()
    open("/sessions/create", "meter")
    await screen.findByRole("heading", { name: "Create session" })
    expect(button("Create session").hasAttribute("disabled")).toBe(false)
    expect(screen.queryByTestId("create-refusal")).toBeNull()
  })
})

describe("blocked create", () => {
  async function form(scenario: string) {
    const server = open("/sessions/create", scenario)
    await screen.findByRole("heading", { name: "Create session" })
    await screen.findByTestId("create-cost")
    return server
  }
  const refusal = () => screen.getByTestId("create-refusal")
  const disabled = () => button("Create session").hasAttribute("disabled")

  it("always says what a session costs", async () => {
    await form("active")
    expect(screen.getByTestId("create-cost").textContent).toBe(
      "This session will use $0.20 an hour while awake and $1.40 a month while it exists.",
    )
    expect(screen.queryByTestId("create-refusal")).toBeNull()
    expect(disabled()).toBe(false)
  })

  it("no payment method", async () => {
    await form("no-card")
    expect(refusal().textContent).toContain("Add a payment method to create a session.")
    expect(within(refusal()).getByRole("button", { name: "Add a card" })).toBeTruthy()
    expect(disabled()).toBe(true)
  })

  it("out of credit", async () => {
    await form("exhausted")
    expect(refusal().textContent).toContain("You are out of credit.")
    expect(within(refusal()).getByRole("button", { name: "Add credit" })).toBeTruthy()
    expect(within(refusal()).getByRole("button", { name: "See plans" })).toBeTruthy()
    expect(disabled()).toBe(true)
  })

  it("the plan's sessions", async () => {
    const { writes } = await form("session-limit")
    expect(refusal().textContent).toContain("Your plan allows 3 sessions. Delete one, or change plan.")
    expect(disabled()).toBe(true)
    fireEvent.submit(document.querySelector("form")!)
    expect(writes()).toHaveLength(0)
  })

  it("the plan's sessions awake at once", async () => {
    await form("awake-limit")
    expect(refusal().textContent).toContain("Your plan runs 2 sessions at once. Stop one, or change plan.")
    expect(disabled()).toBe(true)
  })

  it("shows the server's refusal the same way when it still comes", async () => {
    const server = startBackend({ billing: "active" })
    const real = globalThis.__testFetch!
    let answer = { status: 503, body: { error: "Every desktop is in use right now. Try again in a few minutes.", code: "at_capacity" } as object }
    globalThis.__testFetch = async (input, init) =>
      String(input) === "/api/sessions" && init?.method === "POST"
        ? new Response(JSON.stringify(answer.body), { status: answer.status, headers: { "Content-Type": "application/json" } })
        : real(input, init)
    renderAt("/sessions/create", [{ path: "*", element: <App /> }])
    await screen.findByTestId("create-cost")
    fireEvent.click(button("Create session"))
    await waitFor(() => expect(refusal().textContent).toContain("Every desktop is in use right now."))
    expect(disabled()).toBe(false) // trying again is right

    answer = { status: 402, body: { error: "You are out of credit. Add credit or change plan to continue.", code: "out_of_credit" } }
    fireEvent.click(button("Create session"))
    await waitFor(() => expect(refusal().textContent).toContain("You are out of credit."))
    expect(within(refusal()).getByRole("button", { name: "Add credit" })).toBeTruthy()
    void server
  })
})

describe("blocked wake on the session page", () => {
  it("out of credit", async () => {
    const { sessionNamed } = open("/", "exhausted")
    await banner()
    cleanup()
    renderAt(`/sessions/${sessionNamed("research").id}`, [{ path: "*", element: <App /> }])
    const place = await screen.findByTestId("blocked-wake")
    expect(place.textContent).toContain("Asleep: out of credit")
    expect(place.textContent).toContain("This session is kept as it was. It can wake once you have credit.")
    expect(within(place).getByRole("button", { name: "Add credit" })).toBeTruthy()
    expect(within(place).getByRole("button", { name: "See plans" })).toBeTruthy()
    expect(screen.queryByTestId("viewer")).toBeNull()
    expect(button("Wake").getAttribute("aria-disabled")).toBe("true")
    expect(button("Delete").hasAttribute("disabled")).toBe(false)
  })

  it("no payment method", async () => {
    const { sessionNamed } = open("/", "no-card+deleting")
    await banner()
    cleanup()
    renderAt(`/sessions/${sessionNamed("research").id}`, [{ path: "*", element: <App /> }])
    const place = await screen.findByTestId("blocked-wake")
    expect(place.textContent).toContain("Asleep: no payment method")
    expect(place.textContent).toContain("It can wake once you add a card.")
    expect(place.textContent).toMatch(/Deleted on \d+ \w+ unless you add credit\./)
    expect(within(place).getByRole("button", { name: "Add a card" })).toBeTruthy()
    expect(within(place).queryByRole("button", { name: "Add credit" })).toBeNull()
  })

  it("suspended", async () => {
    // The page alone: under the app a blocked account sees only the alert.
    const { sessionNamed } = startBackend({ billing: "blocked" })
    const id = sessionNamed("research").id
    renderAt(`/sessions/${id}`, [{ path: "/sessions/:id", element: <SessionDetail id={id} /> }])
    const place = await screen.findByTestId("blocked-wake")
    expect(place.textContent).toBe("Suspended.")
    expect(within(place).queryByRole("button")).toBeNull()
  })

  it("wakes as any session once the credit is back, whatever put it to sleep", async () => {
    const server = open("/", "exhausted")
    await banner()
    const id = server.sessionNamed("research").id
    cleanup()
    // Credit arrives; the session still says it was stopped for credit.
    server.backend.billing.set("active")
    const s = server.sessionNamed("research")
    Object.assign(s, { state: "asleep", stoppedBy: "credit" })
    renderAt(`/sessions/${s.id}`, [{ path: "*", element: <App /> }])
    await screen.findByRole("button", { name: "Wake" })
    expect(screen.queryByTestId("blocked-wake")).toBeNull()
    expect(button("Wake").getAttribute("aria-disabled")).toBeNull()
    void id
  })
})

describe("the billing page", () => {
  it("shows the credit, what is using it, the plan, the card and the usage", async () => {
    open("/billing", "active")
    expect((await screen.findByTestId("balance")).textContent).toBe("$32.84")
    const text = () => document.body.textContent!
    expect(text()).toContain("$0.41 an hour · about 79 hours left at this rate (an estimate)")
    expect(text()).toContain("2 sessions awake, 6 sessions kept")
    expect(text()).toMatch(/Plan credit\$31\.54, until \d+ \w+/)
    expect(text()).toMatch(/Sign-up credit\$1\.30, until \d+ \w+/)
    expect(text()).toContain("Purchased credit$0.00")
    expect(text()).toContain("Plan credit used this period")
    expect(text()).toContain("$12.46 of $44.00")
    expect(text()).toContain("Credit is used at $0.20 for each hour a session is awake, and $1.40 a month for each session you keep (its 5 GB disk)")

    await screen.findByTestId("plan-pro")
    expect(screen.getByTestId("plan-pro").textContent).toBe("Pro ●")
    expect(text()).toContain("$5 a month")
    expect(text()).toContain("$10 of credit a month")
    expect(text()).toContain("about 42 hours awake with one session kept (an estimate)")
    expect(text()).toContain("10 sessions")
    expect(text()).toContain("4 awake at once")
    expect(screen.getAllByText("Current plan")).toHaveLength(1)
    expect(screen.getAllByRole("button", { name: /^Change or cancel plan/ })).toHaveLength(2)
    expect(text()).toMatch(/Renews \d+ \w+ \d{4}/)

    expect(text()).toContain("Visa ···· 4242, expires 08/28")
    expect(button("Manage cards")).toBeTruthy()
    expect(screen.queryByText("Auto-recharge")).toBeNull() // behind its switch

    const usage = await screen.findByRole("table", { name: "Usage by session" })
    const rows = within(usage).getAllByRole("row").slice(1)
    expect(rows).toHaveLength(6)
    expect(rows[0].textContent).toMatch(/^research\d+ h \d\d\$\d+\.\d\d\$\d+\.\d\d\$\d+\.\d\d$/)
    expect(screen.getByRole("figure").querySelectorAll(".wf-chart-day").length).toBeGreaterThan(5)
  })

  it("pay as you go: Subscribe goes to Checkout with the plan", async () => {
    const { sent } = open("/billing", "payg")
    await screen.findByTestId("plan-payg")
    expect(screen.queryByText("Plan credit used this period")).toBeNull()
    fireEvent.click(button("Subscribe to Starter"))
    await waitFor(() => expect(went).toHaveLength(1))
    expect(sent.find(r => r.path === "/api/billing/checkout")?.body).toEqual({ item: "cu_starter_monthly_v1" })
  })

  it("says so when the ledger is not set up, is stale, or the plan is ending", async () => {
    open("/billing", "pending")
    expect(await screen.findByText("Setting up your account")).toBeTruthy()
    expect(screen.queryByTestId("balance")).toBeNull()
    cleanup()
    open("/billing", "stale")
    expect(await screen.findByText(/Billing is not being updated right now\. The numbers are from \d\d:\d\d\./)).toBeTruthy()
    cleanup()
    open("/billing", "ending")
    expect(await screen.findByText(/Your plan ends on \d+ \w+ \d{4}\. After that you pay as you go from your credit\./)).toBeTruthy()
    expect(button("Keep plan")).toBeTruthy()
    expect(document.body.textContent).toMatch(/Ends \d+ \w+ \d{4}/)
    cleanup()
    open("/billing", "past-due")
    expect(await screen.findByText("Payment failed")).toBeTruthy()
  })

  it("turns auto-recharge on only with the agreement", async () => {
    const { sent } = open("/billing", "auto")
    const toggle = await screen.findByRole("checkbox", { name: "Auto-recharge" })
    expect(screen.queryByRole("button", { name: "Turn on auto-recharge" })).toBeNull()
    fireEvent.click(toggle)
    const page = createWrapper(document.body)
    const select = page.findSelect('[class*="awsui_root"]:has([aria-label="Amount to buy"])') ?? page.findAllSelects().find(s => s.getElement().textContent?.includes("Choose an amount"))!
    select.openDropdown()
    select.selectOptionByValue("credit-20")
    const [threshold, cap] = page.findAllInputs().slice(-2)
    threshold.setInputValue("2")
    cap.setInputValue("50")
    const agreement = await screen.findByRole("checkbox", { name: /When my balance falls below/ })
    expect(agreement.closest("label, span, div")!.parentElement!.textContent).toContain(
      "When my balance falls below $2, charge my saved card $20 for $20 of credit, as often as needed but not more than $50 in a calendar month, until I turn this off here. Each charge appears in my invoices.",
    )
    expect(button("Turn on auto-recharge").hasAttribute("disabled")).toBe(true)
    fireEvent.click(agreement)
    fireEvent.click(button("Turn on auto-recharge"))
    await waitFor(() => expect(sent.some(r => r.method === "PUT")).toBe(true))
    expect(sent.find(r => r.method === "PUT")).toMatchObject({
      path: "/api/billing/auto-recharge",
      body: { enabled: true, pack: "credit-20", thresholdMicros: 2_000_000, monthlyCapCents: 5000, agree: true },
    })
    expect(await screen.findByText("Charged automatically this month: $0 of $50")).toBeTruthy()
  })

  it("deletes the account only when the email address is typed", async () => {
    const { sent } = open("/billing", "active")
    fireEvent.click(await screen.findByRole("button", { name: "Danger zone" }))
    fireEvent.click(button("Delete account"))
    const modal = await screen.findByRole("dialog")
    const confirm = within(modal).getByRole("button", { name: "Delete account" })
    expect(confirm.hasAttribute("disabled")).toBe(true)
    createWrapper(modal).findInput()!.setInputValue("you@example.com")
    fireEvent.click(confirm)
    await waitFor(() => expect(went).toEqual(["/.pomerium/sign_out"]))
    expect(sent.find(r => r.method === "DELETE")).toMatchObject({ path: "/api/account", body: { confirm: "you@example.com" } })
  })
})

describe("coming back from Checkout", () => {
  // A Checkout the user "paid": the mock completes it at the first question.
  async function back(scenario: string, item?: string) {
    const server = startBackend({ billing: scenario, checkoutPolls: 0 })
    const started = await server.backend.fetch("/api/billing/checkout", { method: "POST", body: JSON.stringify(item ? { item } : {}) })
    const { url } = await started.json()
    const view = renderAt(url, [{ path: "*", element: <App /> }])
    return { ...server, ...view }
  }

  it("a new user's card: the gate gives way to the app, with the credit and the next step", async () => {
    const { router } = await back("new")
    const flash = await banner()
    await waitFor(() => expect(flash.textContent).toMatch(/Card saved\. \$5 of credit added, valid until \d+ \w+\./))
    expect(router.state.location.search).toBe("") // the id is used once
    expect(await screen.findByRole("heading", { name: "Billing" })).toBeTruthy()
    expect((await screen.findByTestId("balance")).textContent).toBe("$5.00")
    fireEvent.click(within(flash).getByRole("button", { name: "Create your first desktop" }))
    expect(await screen.findByRole("heading", { name: "Create session" })).toBeTruthy()
  })

  it("a card that earns no credit", async () => {
    await back("new+prepaid")
    const flash = await banner()
    await waitFor(() => expect(flash.textContent).toContain("Card saved. Prepaid cards do not get the sign-up credit."))
    expect(within(flash).getByRole("button", { name: "Add credit" })).toBeTruthy()
  })

  it("a pack and a plan", async () => {
    await back("payg", "cu_credit_20_v1")
    await waitFor(async () => expect((await banner()).textContent).toContain("$20 of credit added."))
    cleanup()
    await back("payg", "cu_starter_monthly_v1")
    await waitFor(async () => expect((await banner()).textContent).toContain("You are on Starter. $10 of credit added."))
  })

  it("says nothing when there is no checkout to confirm", async () => {
    const { sent } = open("/billing?checkout=not-an-id", "active")
    await screen.findByTestId("balance")
    expect(screen.queryByTestId("billing-banners")).toBeNull()
    expect(sent.some(r => r.path.includes("/checkout/"))).toBe(false)
  })
})

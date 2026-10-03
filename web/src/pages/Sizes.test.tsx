// @vitest-environment jsdom
// A session's size: chosen on the create form, shown on the session and the
// list, changed from the session's page.
import { cleanup, configure, fireEvent, screen, waitFor, within } from "@testing-library/react"
import { afterEach, describe, expect, it, vi } from "vitest"
import { App } from "../App"
import { FakeEditor, presetSource, renderAt, startBackend } from "../test/harness"
import { CreateSession } from "./CreateSession"
import { SessionDetail } from "./SessionDetail"
import { SessionsList } from "./SessionsList"

vi.mock("../components/MonacoEditor", () => ({ default: FakeEditor }))
vi.mock("../components/VncPane", () => ({ VncPane: () => <div data-testid="viewer" /> }))

configure({ asyncUtilTimeout: 5000 })
afterEach(cleanup)

const radio = (name: RegExp) => screen.getByRole("radio", { name }) as HTMLInputElement
const submit = () => fireEvent.click(screen.getByRole("button", { name: "Create session" }))
// The text beside a radio: its label and its description.
const about = (name: RegExp) =>
  (radio(name).getAttribute("aria-describedby") ?? "")
    .split(" ")
    .map(id => document.getElementById(id)?.textContent ?? "")
    .join(" ")

async function openForm(options?: Parameters<typeof startBackend>[0]) {
  const server = startBackend(options)
  const view = renderAt("/sessions/create", [{ path: "/sessions/create", element: <CreateSession /> }])
  await screen.findByRole("heading", { name: "Create session" })
  return { ...server, ...view }
}

async function created(server: { writes: () => { method: string; path: string; body: any }[] }) {
  await waitFor(() => expect(server.writes().some(r => r.path === "/api/sessions")).toBe(true))
  return server.writes().find(r => r.path === "/api/sessions")!.body
}

describe("the size on the create form", () => {
  it("offers the three sizes with their numbers, small chosen", async () => {
    await openForm()
    await screen.findByRole("heading", { name: "Size" })
    expect(radio(/^Small/).checked).toBe(true)
    expect(about(/^Small/)).toContain("1.5 CPU, 2 GiB of memory. Ready in a few seconds.")
    expect(about(/^Medium/)).toContain("2 CPU, 5 GiB of memory. Starts cold")
    expect(about(/^Large/)).toContain("3 CPU, 10 GiB of memory. Starts cold")
    // Billing is off: nothing of money.
    expect(document.body.textContent).not.toMatch(/\$/)
  })

  it("sends no size for small, so the request is what it always was", async () => {
    const server = await openForm()
    await screen.findByRole("heading", { name: "Size" })
    submit()
    expect(await created(server)).not.toHaveProperty("size")
  })

  it("sends the chosen size", async () => {
    const server = await openForm()
    await screen.findByRole("heading", { name: "Size" })
    fireEvent.click(radio(/^Large/))
    fireEvent.change(screen.getByPlaceholderText(/^[a-z]+-[a-z]+$/), { target: { value: "big" } })
    submit()
    expect(await created(server)).toEqual({ name: "big", size: "large", policy: { kind: "rego", source: presetSource("unrestricted") } })
    await waitFor(() => expect(server.sessionNamed("big").size).toBe("large"))
  })

  it("sends it where the backend has no policies too", async () => {
    const server = await openForm({ policies: false, tokens: false })
    await screen.findByRole("heading", { name: "Size" })
    fireEvent.click(radio(/^Medium/))
    fireEvent.change(screen.getByPlaceholderText(/^[a-z]+-[a-z]+$/), { target: { value: "mid" } })
    submit()
    expect(await created(server)).toEqual({ name: "mid", size: "medium" })
  })

  it("says so, and creates nothing, when there is no room for the size", async () => {
    const server = await openForm({ full: ["large"] })
    await screen.findByRole("heading", { name: "Size" })
    fireEvent.click(radio(/^Large/))
    const before = server.backend.sessions.size
    submit()
    // In the form's error, and in its live region for a screen reader.
    expect((await screen.findAllByText(/no capacity for a large session right now/)).length).toBeGreaterThan(0)
    expect(server.backend.sessions.size).toBe(before)
    expect(screen.queryByTestId("landed")).toBeNull()
  })

  it("has no size to choose where the backend has one", async () => {
    await openForm({ sizes: false })
    expect(screen.queryByRole("heading", { name: "Size" })).toBeNull()
    expect(screen.queryByRole("radio", { name: /^Medium/ })).toBeNull()
  })
})

describe("the size with billing on", () => {
  async function form(scenario: string) {
    const server = startBackend({ billing: scenario })
    renderAt("/sessions/create", [{ path: "*", element: <App /> }])
    await screen.findByRole("heading", { name: "Size" })
    await screen.findByTestId("create-cost")
    return server
  }

  it("shows each size's hourly rate, and the cost of the one chosen", async () => {
    await form("active")
    expect(about(/^Small/)).toContain("$0.20 an hour while awake.")
    expect(about(/^Medium/)).toContain("$0.60 an hour while awake.")
    expect(about(/^Large/)).toContain("$1.60 an hour while awake.")
    expect(screen.getByTestId("create-cost").textContent).toBe(
      "This session will use $0.20 an hour while awake and $1.40 a month while it exists.",
    )
    fireEvent.click(radio(/^Medium/))
    expect(screen.getByTestId("create-cost").textContent).toBe(
      "This session will use $0.60 an hour while awake and $1.40 a month while it exists.",
    )
  })

  it("does not offer a size the plan does not include", async () => {
    await form("payg") // pay as you go: small and medium
    expect(radio(/^Large/).disabled).toBe(true)
    expect(about(/^Large/)).toContain("Not included in your plan.")
    expect(radio(/^Medium/).disabled).toBe(false)
  })
})

describe("the size of a session", () => {
  const detail = (id: string) => renderAt(`/sessions/${id}`, [{ path: "/sessions/:id", element: <SessionDetail id={id} /> }])
  const size = () => screen.getByTestId("session-size")
  const patches = (sent: { method: string; path: string; body: any }[]) => sent.filter(r => r.method === "PATCH").map(r => r.body)

  it("is shown, and changed from the session's page: an awake one has it from its next start", async () => {
    const { sessionNamed, sent } = startBackend()
    const id = sessionNamed("research").id
    detail(id)
    await waitFor(() => expect(size().textContent).toContain("size: Small"))
    fireEvent.click(await within(size()).findByRole("button", { name: "change size" }))
    const dialog = await screen.findByRole("dialog")
    // Said before it is done: the next start is a fresh one.
    expect(within(dialog).getByTestId("resize-consequence").textContent).toMatch(
      /keeps running at the size it has.*from its next start.*a fresh one.*Its files are kept/,
    )
    const save = within(dialog).getByRole("button", { name: "Change size" })
    expect(save.hasAttribute("disabled")).toBe(true) // nothing chosen that it does not have
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Large/ }))
    fireEvent.click(save)
    await waitFor(() => expect(patches(sent)).toEqual([{ size: "large" }]))
    await waitFor(() => expect(size().textContent).toContain("size: Small, Large from its next start"))
    expect(sessionNamed("research").size).toBe("small")
  })

  it("of a session that is asleep changes at once, and its saved state goes", async () => {
    const { sessionNamed, sent } = startBackend()
    const asleep = sessionNamed("from-before")
    detail(asleep.id)
    await waitFor(() => expect(size().textContent).toContain("size: Small"))
    fireEvent.click(await within(size()).findByRole("button", { name: "change size" }))
    const dialog = await screen.findByRole("dialog")
    expect(within(dialog).getByTestId("resize-consequence").textContent).toMatch(/that state is dropped/)
    fireEvent.click(within(dialog).getByRole("radio", { name: /^Medium/ }))
    fireEvent.click(within(dialog).getByRole("button", { name: "Change size" }))
    await waitFor(() => expect(patches(sent)).toEqual([{ size: "medium" }]))
    await waitFor(() => expect(size().textContent).toBe("size: Medium change size"))
    expect(asleep.stateSaved).toBe(false)
  })

  it("is a column of the list once some session is not small", async () => {
    const { sessionNamed } = startBackend()
    const view = renderAt("/", [{ path: "/", element: <SessionsList /> }])
    await screen.findByRole("link", { name: "research" })
    expect(screen.queryByRole("columnheader", { name: "Size" })).toBeNull()
    view.unmount()

    sessionNamed("research").size = "large"
    sessionNamed("scratch").pendingSize = "medium"
    renderAt("/", [{ path: "/", element: <SessionsList /> }])
    const row = (await screen.findByRole("link", { name: "research" })).closest("tr")!
    expect(screen.getByRole("columnheader", { name: "Size" })).toBeTruthy()
    expect(row.textContent).toContain("Large")
    expect(screen.getByRole("link", { name: "scratch" }).closest("tr")!.textContent).toContain("Small, Medium from its next start")
  })

  it("is not shown by a backend from before sizes", async () => {
    const { sessionNamed } = startBackend({ sizes: false })
    detail(sessionNamed("research").id)
    await screen.findByText(/^created /)
    expect(screen.queryByTestId("session-size")).toBeNull()
  })
})

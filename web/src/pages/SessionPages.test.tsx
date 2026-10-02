// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor, within } from "@testing-library/react"
import { afterEach, describe, expect, it, vi } from "vitest"
import { renderAt, startBackend } from "../test/harness"
import { SessionDetail } from "./SessionDetail"
import { SessionsList } from "./SessionsList"

// The viewer needs a real browser (canvas, websockets).
vi.mock("../components/VncPane", () => ({ VncPane: () => <div data-testid="viewer" /> }))

afterEach(cleanup)

describe("sessions list", () => {
  it("shows each policy, tags the ones managed as code, and goes to the create page", async () => {
    const { sessionNamed } = startBackend()
    renderAt("/", [{ path: "/", element: <SessionsList /> }])
    const row = (await screen.findByRole("link", { name: "ci-runner" })).closest("tr")!
    expect(within(row).getByText("as code")).toBeTruthy()
    expect(row.textContent).toContain("Rego, v1, in force")
    const plain = screen.getByRole("link", { name: "research" }).closest("tr")!
    expect(within(plain).queryByText("as code")).toBeNull()
    expect(screen.getByRole("link", { name: "from-before" }).closest("tr")!.textContent).toContain("none (created before policies)")
    expect(within(row).getByRole("button", { name: `Copy the MCP URL of ${sessionNamed("ci-runner").name}` })).toBeTruthy()

    expect(screen.queryByRole("dialog")).toBeNull() // the modal is gone
    fireEvent.click(screen.getByRole("button", { name: "Create session" }))
    await waitFor(() => expect(screen.getByTestId("landed").textContent).toMatch(/^\/sessions\/create/))
  })

  it("has no policy column where the backend has no policies", async () => {
    startBackend({ policies: false, tokens: false })
    renderAt("/", [{ path: "/", element: <SessionsList /> }])
    await screen.findByRole("link", { name: "research" })
    expect(screen.queryByRole("columnheader", { name: "Policy" })).toBeNull()
    expect(screen.queryByText("as code")).toBeNull()
    expect(screen.queryByRole("link", { name: "API tokens" })).toBeNull()
  })
})

describe("sleep, wake and stop", () => {
  const list = () => renderAt("/", [{ path: "/", element: <SessionsList /> }])
  const rowOf = async (name: string) => (await screen.findByRole("link", { name })).closest("tr")!
  const state = (row: HTMLElement) => row.querySelector(".wf-state")!.textContent
  const posts = (sent: { method: string; path: string }[]) => sent.filter(r => r.method !== "GET").map(r => `${r.method} ${r.path}`)

  it("puts a running session to sleep from the list, and wakes it", async () => {
    const { sessionNamed, sent } = startBackend()
    const id = sessionNamed("research").id
    list()
    const row = await rowOf("research")
    expect(state(row)).toBe("running")
    expect(within(row).queryByRole("button", { name: "Stop" })).toBeNull()
    fireEvent.click(within(row).getByRole("button", { name: "Sleep" }))
    await within(row).findByRole("button", { name: "Wake" })
    expect(state(row)).toBe("asleep")
    expect(posts(sent)).toEqual([`POST /api/sessions/${id}/sleep`])

    fireEvent.click(within(row).getByRole("button", { name: "Wake" }))
    await within(row).findByRole("button", { name: "Sleep" })
    expect(state(row)).toBe("running")
    expect(posts(sent)).toEqual([`POST /api/sessions/${id}/sleep`, `POST /api/sessions/${id}/wake`])
    // The rest of the row is where it was.
    expect(within(row).getByRole("button", { name: "Copy the MCP URL of research" })).toBeTruthy()
    expect(within(row).getByRole("button", { name: "Delete" })).toBeTruthy()
  })

  it("shows the snapshot being taken, and takes no second click meanwhile", async () => {
    const { backend, sessionNamed, sent } = startBackend()
    const id = sessionNamed("research").id
    // The sleep answers only when the test lets it: the snapshot takes seconds.
    let finish = () => {}
    const inner = globalThis.__testFetch!
    globalThis.__testFetch = async (input, init) => {
      if (String(input).endsWith("/sleep")) await new Promise<void>(done => (finish = done))
      return inner(input, init)
    }
    list()
    const row = await rowOf("research")
    fireEvent.click(within(row).getByRole("button", { name: "Sleep" }))
    const saving = await within(row).findByRole("button", { name: /Saving state/ })
    expect((saving as HTMLButtonElement).disabled).toBe(true)
    fireEvent.click(saving)
    expect(backend.sessions.get(id)!.state).toBe("running")
    finish()
    await within(row).findByRole("button", { name: "Wake" })
    expect(posts(sent)).toEqual([`POST /api/sessions/${id}/sleep`])
  })

  it("keeps Stop as the second choice, and says a stopped session starts fresh", async () => {
    const { sessionNamed, sent } = startBackend()
    const id = sessionNamed("research").id
    list()
    const row = await rowOf("research")
    fireEvent.click(within(row).getByRole("button", { name: "More actions for research" }))
    fireEvent.click(await screen.findByRole("menuitem", { name: /Stop without saving state/ }))
    await within(row).findByRole("button", { name: "Start" })
    expect(state(row)).toBe("stopped: starts fresh")
    expect(sent.find(r => r.method === "PATCH")).toMatchObject({ path: `/api/sessions/${id}`, body: { action: "stop" } })
    // Nothing saved, so nothing to discard: Start is the only choice.
    expect(within(row).queryByRole("button", { name: "More actions for research" })).toBeNull()
  })

  it("says so when a sleep saved no state", async () => {
    startBackend({ snapshots: false })
    list()
    const row = await rowOf("research")
    fireEvent.click(within(row).getByRole("button", { name: "Sleep" }))
    await within(row).findByRole("button", { name: "Wake" })
    expect(state(row)).toBe("asleep: state not saved")
  })

  it("has the same buttons on the session's page, with the state in words", async () => {
    const { sessionNamed, sent } = startBackend()
    const id = sessionNamed("research").id
    renderAt(`/sessions/${id}`, [{ path: "/sessions/:id", element: <SessionDetail id={id} /> }])
    await screen.findByTestId("viewer")
    fireEvent.click(screen.getByRole("button", { name: "Sleep" }))
    await screen.findByRole("button", { name: "Wake" })
    expect(screen.queryByTestId("viewer")).toBeNull()
    expect(document.body.textContent).toContain("Asleep, with its state saved. It wakes as it was when you or an agent uses it.")
    expect(document.querySelector(".wf-state")!.textContent).toBe("asleep")
    for (const name of [/^Copy the MCP URL/, "Delete"]) expect(screen.getByRole("button", { name })).toBeTruthy()

    // Asleep with its state saved: Stop discards it.
    fireEvent.click(screen.getByRole("button", { name: "More actions for research" }))
    fireEvent.click(await screen.findByRole("menuitem", { name: /Stop and discard saved state/ }))
    await screen.findByRole("button", { name: "Start" })
    expect(document.body.textContent).toContain("Stopped. Its state was not saved: it starts fresh")
    fireEvent.click(screen.getByRole("button", { name: "Start" }))
    await screen.findByTestId("viewer")
    expect(posts(sent)).toEqual([`POST /api/sessions/${id}/sleep`, `PATCH /api/sessions/${id}`, `POST /api/sessions/${id}/wake`])
  })

  it("cannot put a session to sleep before it runs, and shows the server's refusal", async () => {
    const { backend, sessionNamed } = startBackend({ policies: false, tokens: false })
    const s = sessionNamed("research")
    s.state = "starting"
    list()
    const row = await rowOf("research")
    expect(within(row).getByRole("button", { name: "Sleep" }).getAttribute("aria-disabled")).toBe("true")
    // Stopped by someone else since the page last asked.
    backend.sessions.get(sessionNamed("ci-runner").id)!.state = "stopped"
    fireEvent.click(within(await rowOf("ci-runner")).getByRole("button", { name: "Sleep" }))
    await waitFor(() => expect(document.body.textContent).toContain("⚠ session is stopped, with no running state to save"))
  })
})

describe("session page", () => {
  it("has Browser and Policy tabs under the title row", async () => {
    const { sessionNamed } = startBackend()
    const id = sessionNamed("research").id
    renderAt(`/sessions/${id}`, [{ path: "/sessions/:id", element: <SessionDetail id={id} /> }])
    expect(await screen.findByTestId("viewer")).toBeTruthy()
    expect(screen.getByRole("link", { name: "Rego, v1, in force" })).toBeTruthy()
    expect(screen.getByRole("button", { name: /^Copy the MCP URL/ })).toBeTruthy()
    fireEvent.click(screen.getByRole("tab", { name: "Policy" }))
    expect(await screen.findByRole("button", { name: "Edit" })).toBeTruthy()
    expect(screen.queryByTestId("viewer")).toBeNull()
    // The title row, with the state and the policy, stays in view.
    expect(screen.getByRole("link", { name: "Rego, v1, in force" })).toBeTruthy()
    expect(screen.getByRole("button", { name: /^Copy the MCP URL/ })).toBeTruthy()
  })

  it("is the browser alone, with no tabs and no errors, where the backend has no policies", async () => {
    const { sessionNamed, sent } = startBackend({ policies: false, tokens: false })
    const id = sessionNamed("research").id
    renderAt(`/sessions/${id}`, [{ path: "/sessions/:id", element: <SessionDetail id={id} /> }])
    expect(await screen.findByTestId("viewer")).toBeTruthy()
    expect(screen.queryByRole("tab")).toBeNull()
    expect(screen.queryByText(/Policy/)).toBeNull()
    expect(document.body.textContent).not.toContain("⚠")
    expect(sent.some(r => r.path.includes("/policy"))).toBe(false)
  })
})

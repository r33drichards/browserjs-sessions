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

describe("session page", () => {
  it("has Browser and Policy tabs under the title row", async () => {
    const { sessionNamed } = startBackend()
    const id = sessionNamed("research").id
    renderAt(`/sessions/${id}`, [{ path: "/sessions/:id", element: <SessionDetail id={id} /> }])
    expect(await screen.findByTestId("viewer")).toBeTruthy()
    expect(screen.getByRole("link", { name: "JSON, v1, in force" })).toBeTruthy()
    expect(screen.getByRole("button", { name: /^Copy the MCP URL/ })).toBeTruthy()
    fireEvent.click(screen.getByRole("tab", { name: "Policy" }))
    expect(await screen.findByRole("button", { name: "Edit" })).toBeTruthy()
    expect(screen.queryByTestId("viewer")).toBeNull()
    // The title row, with the state and the policy, stays in view.
    expect(screen.getByRole("link", { name: "JSON, v1, in force" })).toBeTruthy()
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

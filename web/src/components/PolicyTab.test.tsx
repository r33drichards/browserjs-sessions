// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react"
import { afterEach, describe, expect, it } from "vitest"
import { presetSource, renderAt, startBackend } from "../test/harness"
import { PolicyTab } from "./PolicyTab"

afterEach(cleanup)

type Server = ReturnType<typeof startBackend>

// `prepare` changes the session before the page asks for it.
function open(name: string, prepare?: (session: ReturnType<Server["sessionNamed"]>) => void) {
  const server = startBackend()
  const session = server.sessionNamed(name)
  prepare?.(session)
  const summary = session.unsupported
    ? ({ state: "unsupported" } as const)
    : { kind: session.policy!.kind, version: session.policy!.version, state: session.policy!.state, management: session.policy!.management }
  renderAt("/", [{ path: "/", element: <PolicyTab sessionId={session.id} sessionName={session.name} summary={summary} /> }])
  return { ...server, session }
}

const button = (name: string) => screen.queryByRole("button", { name })

describe("policy tab", () => {
  it("shows a policy managed here with its write actions and state line", async () => {
    open("research")
    await waitFor(() => expect(screen.getByTestId("policy-status").textContent).toContain("in force (2/2)"))
    expect(button("Edit")).toBeTruthy()
    expect(button("Reset")).toBeTruthy()
    expect(button("Copy from session")).toBeTruthy()
    expect(button("Manage here instead")).toBeNull()
    expect(screen.getByText("this editor", { exact: false })).toBeTruthy()
    // The source, read-only. It is the module: there is no second pane.
    expect(screen.getByLabelText("policy.rego, read-only").textContent).toBe(presetSource("one-site"))
    expect(screen.queryByLabelText(/Generated Rego/)).toBeNull()
    expect(screen.queryByLabelText("Policy in force, read-only")).toBeNull()
    expect(screen.queryByRole("list", { name: "Warnings" })).toBeNull()
    expect(screen.getByText(/may do in the browser, on the desktop and in the shell/)).toBeTruthy()
    expect(screen.getByText("Rego")).toBeTruthy()
  })

  it("is read-only for a policy managed as code: the link, no write actions", async () => {
    const { session } = open("ci-runner")
    expect(await screen.findByText("This policy is managed as code")).toBeTruthy()
    const link = screen.getByRole("link", { name: session.policy!.management.managed_url })
    expect(link.getAttribute("href")).toBe("https://github.com/me/infra/blob/main/browserjs/main.tf")
    expect(link.getAttribute("rel")).toContain("noopener")
    expect(button("Edit")).toBeNull()
    expect(button("Reset")).toBeNull()
    expect(button("Copy from session")).toBeNull()
    expect(button("View source")).toBeTruthy()
    expect(screen.getByText(/by token "ci"/)).toBeTruthy()
    expect(screen.getByLabelText("policy.rego, read-only").textContent).toContain("allow_tool_call")
  })

  it("switches back with Manage here instead, which returns the write actions", async () => {
    const { session, writes } = open("ci-runner")
    fireEvent.click(await screen.findByRole("button", { name: "Manage here instead" }))
    // Opens on the editor: one more click.
    expect((screen.getByRole("radio", { name: /This editor/ }) as HTMLInputElement).checked).toBe(true)
    fireEvent.click(screen.getByRole("button", { name: "Save management" }))
    await waitFor(() => expect(button("Edit")).toBeTruthy())
    expect(writes()).toEqual([expect.objectContaining({ method: "PUT", path: `/api/sessions/${session.id}/policy/management`, body: { mode: "editor" } })])
    expect(screen.queryByText("This policy is managed as code")).toBeNull()
    expect(button("Manage here instead")).toBeNull()
  })

  it("hands a policy over to code only with an https link", async () => {
    const { session, writes } = open("research")
    fireEvent.click(await screen.findByRole("button", { name: "Change" }))
    fireEvent.click(screen.getByRole("radio", { name: /Code/ }))
    fireEvent.click(screen.getByRole("button", { name: "Save management" }))
    expect(await screen.findByText("Enter the link to where the policy is managed.")).toBeTruthy()
    expect(writes()).toHaveLength(0)
    fireEvent.change(screen.getByPlaceholderText("https://"), { target: { value: "https://example.com/main.tf" } })
    fireEvent.click(screen.getByRole("button", { name: "Save management" }))
    expect(await screen.findByText("This policy is managed as code")).toBeTruthy()
    expect(writes()[0]).toMatchObject({ path: `/api/sessions/${session.id}/policy/management`, body: { mode: "iac", managed_url: "https://example.com/main.tf" } })
    expect(button("Edit")).toBeNull()
  })

  it("resets to the unrestricted policy after a confirmation", async () => {
    const { session, writes } = open("research")
    fireEvent.click(await screen.findByRole("button", { name: "Reset" }))
    expect(writes()).toHaveLength(0)
    expect(screen.getByText(/an agent may then use the browser,\s+the desktop and the shell/)).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "Reset policy" }))
    expect(await screen.findByText("No restrictions: an agent may use the browser, the desktop and the shell.")).toBeTruthy()
    expect(writes()).toEqual([expect.objectContaining({ method: "DELETE", path: `/api/sessions/${session.id}/policy` })])
    expect(screen.getByText("Policy reset and in force (v2).")).toBeTruthy()
  })

  it("says a policy is loading until every replica has it", async () => {
    open("just-saved")
    await waitFor(() => expect(screen.getByTestId("policy-status").textContent).toContain("loading (1/2)"))
  })

  it("shows why a policy does not compile, with the way to fix it", async () => {
    open("broken")
    expect(await screen.findByText("This policy does not compile")).toBeTruthy()
    expect(screen.getByText(/undefined function http\.send/)).toBeTruthy()
    expect(button("Edit policy")).toBeTruthy()
    // The saved source, and under it the one that is still in force.
    expect(screen.getByLabelText("policy.rego, read-only").textContent).toContain("http.send")
    expect(screen.getByLabelText("Policy in force, read-only").textContent).toBe(presetSource("form-filling"))
  })

  it("shows the warnings of the saved policy, which have no position", async () => {
    open("research", s => (s.policy!.source += '\nallow_tool_call if input.server == "exec"\n'))
    const warnings = await screen.findByRole("list", { name: "Warnings" })
    expect(warnings.textContent).toMatch(/^this policy restricts what browser_execute may do and allows the shell/)
    expect(screen.getByText("This policy has a warning")).toBeTruthy()
  })

  it("explains a session from before policies and offers nothing to edit", async () => {
    const { sent } = open("from-before")
    expect(await screen.findByText("This session can't have a policy")).toBeTruthy()
    expect(button("Edit")).toBeNull()
    expect(button("Reset")).toBeNull()
    // It has no policy object: none is asked for.
    await new Promise(r => setTimeout(r, 20))
    expect(sent.some(r => r.path.endsWith("/policy"))).toBe(false)
  })
})

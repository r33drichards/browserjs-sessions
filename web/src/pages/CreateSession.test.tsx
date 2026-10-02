// @vitest-environment jsdom
import createWrapper from "@cloudscape-design/components/test-utils/dom"
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react"
import { afterEach, describe, expect, it, vi } from "vitest"
import { FakeEditor, presetSource, renderAt, startBackend } from "../test/harness"
import { CreateSession, policyForChoice } from "./CreateSession"

vi.mock("../components/MonacoEditor", () => ({ default: FakeEditor }))

afterEach(cleanup)

const routes = [{ path: "/sessions/create", element: <CreateSession /> }]

async function openForm(options?: Parameters<typeof startBackend>[0]) {
  const server = startBackend(options)
  const view = renderAt("/sessions/create", routes)
  await screen.findByRole("heading", { name: "Create session" })
  return { ...server, ...view }
}

const submit = () => fireEvent.click(screen.getByRole("button", { name: "Create session" }))
const choose = (name: RegExp) => fireEvent.click(screen.getByRole("radio", { name }))

async function created(server: { writes: () => { method: string; path: string; body: any }[] }) {
  await waitFor(() => expect(server.writes().some(r => r.path === "/api/sessions")).toBe(true))
  return server.writes().find(r => r.path === "/api/sessions")!.body
}

describe("create session", () => {
  it("creates untouched: the placeholder name and the unrestricted policy", async () => {
    const server = await openForm()
    const placeholder = (screen.getByPlaceholderText(/^[a-z]+-[a-z]+$/) as HTMLInputElement).placeholder
    expect((screen.getByRole("radio", { name: /No restrictions/ }) as HTMLInputElement).checked).toBe(true)
    submit()
    expect(await created(server)).toEqual({ name: placeholder, policy: { kind: "json", source: presetSource("unrestricted") } })
    // Lands on the new session's page with the message.
    await waitFor(() => expect(screen.getByTestId("landed").textContent).toMatch(new RegExp(`^/sessions/s-\\w+ Session ${placeholder} created`)))
  })

  it("uses the typed name", async () => {
    const server = await openForm()
    fireEvent.change(screen.getByPlaceholderText(/^[a-z]+-[a-z]+$/), { target: { value: "  my browser " } })
    submit()
    expect((await created(server)).name).toBe("my browser")
  })

  it("sends the chosen preset", async () => {
    const server = await openForm()
    choose(/No scripting/)
    submit()
    expect((await created(server)).policy).toEqual({ kind: "json", source: presetSource("no-scripting") })
  })

  it("copies the policy of the chosen session", async () => {
    const server = await openForm()
    choose(/Copy from a session/)
    submit() // nothing chosen yet
    expect(await screen.findByText("Choose the session to copy the policy from.")).toBeTruthy()
    expect(server.writes()).toHaveLength(0)

    const select = createWrapper().findSelect()!
    select.openDropdown()
    select.selectOptionByValue(server.sessionNamed("research").id)
    submit()
    expect((await created(server)).policy).toEqual({ kind: "json", source: presetSource("one-site") })
    // A session from before policies has none to copy.
    expect(server.sent.some(r => r.path.includes(server.sessionNamed("from-before").id))).toBe(false)
  })

  it("asks for an https link before creating a session managed as code", async () => {
    const server = await openForm()
    choose(/Managed as code/)
    submit()
    expect(await screen.findByText("Enter the link to where the policy is managed.")).toBeTruthy()
    fireEvent.change(screen.getByPlaceholderText(/^https:\/\/github/), { target: { value: "http://example.com/main.tf" } })
    submit()
    expect(await screen.findByText("The link must start with https://.")).toBeTruthy()
    expect(server.writes()).toHaveLength(0)

    fireEvent.change(screen.getByPlaceholderText(/^https:\/\/github/), { target: { value: "https://example.com/main.tf" } })
    submit()
    expect((await created(server)).policy).toEqual({
      kind: "json",
      source: presetSource("unrestricted"),
      management: { mode: "iac", managed_url: "https://example.com/main.tf" },
    })
  })

  it("writes a policy in the split panel and creates with it", async () => {
    const server = await openForm()
    choose(/Write a policy/)
    submit() // nothing written yet
    expect(await screen.findByText("Write the policy first: choose Edit policy.")).toBeTruthy()
    expect(server.writes()).toHaveLength(0)

    fireEvent.click(screen.getByRole("button", { name: "Edit policy" }))
    const editor = await screen.findByLabelText("JSON policy editor")
    const bad = '{\n  "version": 1,\n  "allow": { "operations": ["clik"] }\n}'
    fireEvent.change(editor, { target: { value: bad } })
    fireEvent.click(screen.getByRole("button", { name: "Use this policy" }))
    expect(await screen.findByText("Fix the errors in the policy before using it.")).toBeTruthy()
    expect(screen.getByTestId("custom-summary").textContent).toBe("No policy written yet")

    const good = '{\n  "version": 1,\n  "allow": { "operations": ["click"] }\n}'
    fireEvent.change(screen.getByLabelText("JSON policy editor"), { target: { value: good } })
    fireEvent.click(screen.getByRole("button", { name: "Use this policy" }))
    await waitFor(() => expect(screen.getByTestId("custom-summary").textContent).toBe("JSON · 4 lines · valid"))
    submit()
    expect((await created(server)).policy).toEqual({ kind: "json", source: good })
  })

  it("shows the errors of a policy the backend refuses, and creates nothing", async () => {
    const server = await openForm()
    // The presets as a backend with a broken one would serve them.
    server.backend.sessions.clear()
    choose(/No scripting/)
    globalThis.__testFetch = async (input, init) =>
      String(input) === "/api/sessions" && init?.method === "POST"
        ? new Response(JSON.stringify({ error: "the policy does not validate", errors: [{ row: 4, col: 21, code: "x", message: 'unknown operation "clik"' }] }), {
            status: 422,
            headers: { "Content-Type": "application/json" },
          })
        : server.backend.fetch(input, init)
    submit()
    expect(await screen.findByText('4:21 unknown operation "clik"', { normalizer: s => s.replace(/\s+/g, " ") })).toBeTruthy()
    expect(screen.getByText("The policy does not validate. No session was created.")).toBeTruthy()
    expect(server.backend.sessions.size).toBe(0)
    expect(screen.queryByTestId("landed")).toBeNull()
  })

  it("is today's form where the backend has no policies", async () => {
    const server = await openForm({ policies: false, tokens: false })
    expect(screen.queryByRole("heading", { name: "Policy" })).toBeNull()
    expect(screen.queryByRole("radio")).toBeNull()
    expect(screen.queryByRole("alert")).toBeNull()
    const placeholder = (screen.getByPlaceholderText(/^[a-z]+-[a-z]+$/) as HTMLInputElement).placeholder
    submit()
    expect(await created(server)).toEqual({ name: placeholder })
    await waitFor(() => expect(screen.getByTestId("landed").textContent).toMatch(/^\/sessions\/s-/))
  })

  it("asks before leaving with changes, and not without", async () => {
    await openForm()
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }))
    await waitFor(() => expect(screen.getByTestId("landed").textContent).toMatch(/^\/ /))
    cleanup()

    await openForm()
    fireEvent.change(screen.getByPlaceholderText(/^[a-z]+-[a-z]+$/), { target: { value: "kept" } })
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }))
    expect(await screen.findByText(/The changes that you made won't be saved/)).toBeTruthy()
    expect(screen.queryByTestId("landed")).toBeNull()
    fireEvent.click(screen.getByRole("button", { name: "Leave" }))
    await waitFor(() => expect(screen.getByTestId("landed").textContent).toMatch(/^\/ /))
  })
})

describe("policyForChoice", () => {
  const presets = [
    { id: "unrestricted", title: "No restrictions", description: "", kind: "json" as const, source: "U" },
    { id: "no-scripting", title: "No scripting", description: "", kind: "json" as const, source: "N" },
  ]
  const base = { presets, managedUrl: "" }

  it("gives each choice its policy", () => {
    expect(policyForChoice({ ...base, choice: "preset:no-scripting" })).toEqual({ kind: "json", source: "N" })
    expect(policyForChoice({ ...base, choice: "copy", copied: { kind: "rego", source: "R" } })).toEqual({ kind: "rego", source: "R" })
    expect(policyForChoice({ ...base, choice: "custom", custom: { kind: "rego", source: "C" } })).toEqual({ kind: "rego", source: "C" })
    expect(policyForChoice({ ...base, choice: "iac", managedUrl: " https://x.example/a " })).toEqual({
      kind: "json",
      source: "U",
      management: { mode: "iac", managed_url: "https://x.example/a" },
    })
    // Only a policy managed as code names a mode; the rest are the editor's by default.
    expect(policyForChoice({ ...base, choice: "preset:unrestricted" })).not.toHaveProperty("management")
  })
})

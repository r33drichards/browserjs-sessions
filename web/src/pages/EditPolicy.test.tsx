// @vitest-environment jsdom
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react"
import { afterEach, describe, expect, it, vi } from "vitest"
import { FakeEditor, presetSource, renderAt, startBackend } from "../test/harness"
import { EditPolicy } from "./EditPolicy"

vi.mock("../components/MonacoEditor", () => ({ default: FakeEditor }))

afterEach(cleanup)

async function open(name: string) {
  const server = startBackend()
  const session = server.sessionNamed(name)
  renderAt(`/sessions/${session.id}/policy/edit`, [{ path: "/sessions/:id/policy/edit", element: <EditPolicy id={session.id} /> }])
  return { ...server, session }
}

const editor = () => screen.findByLabelText("JSON policy editor") as Promise<HTMLTextAreaElement>
const save = () => fireEvent.click(screen.getByRole("button", { name: "Save policy" }))
const landed = () => screen.getByTestId("landed").textContent

describe("edit policy", () => {
  it("saves with the version it started from and returns to the Policy tab", async () => {
    const { session, writes } = await open("research")
    expect((await editor()).value).toBe(presetSource("one-site"))
    fireEvent.change(await editor(), { target: { value: presetSource("no-scripting") } })
    save()
    await waitFor(() => expect(landed()).toBe(`/sessions/${session.id}?tab=policy Policy saved and in force (v2)`))
    const [put] = writes()
    expect(put).toMatchObject({ method: "PUT", path: `/api/sessions/${session.id}/policy`, body: { kind: "json", source: presetSource("no-scripting") } })
    expect(put.headers.get("If-Match")).toBe('"1"')
  })

  it("says so when the policy is saved but not yet loaded everywhere", async () => {
    const { session } = await open("research")
    fireEvent.change(await editor(), { target: { value: '{ "version": 1, "description": "slow to load", "allow": { "operations": ["*"] } }' } })
    save()
    await waitFor(() => expect(landed()).toBe(`/sessions/${session.id}?tab=policy Policy saved; loading`))
  })

  it("sends nothing when nothing changed", async () => {
    const { session, writes } = await open("research")
    await editor()
    save()
    await waitFor(() => expect(landed()).toBe(`/sessions/${session.id}?tab=policy No changes were made to the policy.`))
    expect(writes()).toHaveLength(0)
  })

  it("keeps the user on the page when the policy changed underneath, with Reload", async () => {
    const { session } = await open("research")
    fireEvent.change(await editor(), { target: { value: presetSource("no-scripting") } })
    session.policy!.version = 5 // someone else saved
    session.policy!.source = presetSource("observe-only")
    save()
    expect(await screen.findByText("This policy changed since you opened it")).toBeTruthy()
    expect(screen.queryByTestId("landed")).toBeNull()
    fireEvent.click(screen.getByRole("button", { name: "Reload" }))
    await waitFor(async () => expect((await editor()).value).toBe(presetSource("observe-only")))
    expect(screen.queryByText("This policy changed since you opened it")).toBeNull()
  })

  it("keeps the user on the page with the errors of a policy that does not validate", async () => {
    const { writes } = await open("research")
    fireEvent.change(await editor(), { target: { value: '{ "version": 1, "allow": { "operations": ["clik"] } }' } })
    save()
    expect(await screen.findByText("The policy does not validate. Nothing was saved.")).toBeTruthy()
    expect(writes()).toHaveLength(1)
    expect(screen.queryByTestId("landed")).toBeNull()
    expect((await screen.findByRole("list", { name: "Problems" })).textContent).toContain('unknown operation "clik"')
  })

  it("asks before leaving with changes", async () => {
    const { session } = await open("research")
    fireEvent.change(await editor(), { target: { value: presetSource("no-scripting") } })
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }))
    expect(await screen.findByText(/The changes that you made won't be saved/)).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "Leave" }))
    await waitFor(() => expect(landed()).toMatch(`/sessions/${session.id}?tab=policy`))
  })

  it("is the read-only view for a policy managed as code, not an error", async () => {
    const { writes } = await open("ci-runner")
    expect(await screen.findByText("Read-only: managed as code")).toBeTruthy()
    expect(screen.getByRole("link", { name: /github\.com\/me\/infra/ })).toBeTruthy()
    expect(screen.getByLabelText("policy.rego, read-only").textContent).toContain("allow_tool_call")
    expect(screen.queryByRole("button", { name: "Save policy" })).toBeNull()
    expect(screen.queryByLabelText(/policy editor$/)).toBeNull()
    // Test still works: it saves nothing.
    fireEvent.click(screen.getByRole("button", { name: "Run test" }))
    expect(await screen.findByText("Allowed")).toBeTruthy()
    expect(writes()).toHaveLength(0)
  })

  it("has nothing to edit for a session from before policies", async () => {
    await open("from-before")
    expect(await screen.findByText("This session can't have a policy")).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Save policy" })).toBeNull()
  })
})

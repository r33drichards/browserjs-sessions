// @vitest-environment jsdom
import createWrapper from "@cloudscape-design/components/test-utils/dom"
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { useState } from "react"
import { afterEach, describe, expect, it, vi } from "vitest"
import { SAMPLES } from "../policy/samples"
import type { PolicySource, Validation } from "../policyApi"
import { FakeEditor, presetSource, presets, startBackend } from "../test/harness"
import type { Problems } from "./PolicyWorkbench"
import { PolicyWorkbench } from "./PolicyWorkbench"

vi.mock("./MonacoEditor", () => ({ default: FakeEditor }))

afterEach(cleanup)

const rego = (source: string): PolicySource => ({ kind: "rego", source })
const BAD = "package browserjs.policy\n\nallow_tool_call if http.send({})\n"
// one-site restricts the browser; this leaves the shell open beside it.
const WITH_SHELL = presetSource("one-site") + '\nallow_tool_call if input.server == "exec"\n'

interface BenchProps {
  start: PolicySource
  refused?: Problems
  readOnly?: boolean
  regoInForce?: string
  withPresets?: boolean
  onValidation?: (v: Validation | null) => void
}

function Bench({ start, withPresets, ...props }: BenchProps) {
  const [draft, setDraft] = useState(start)
  return <PolicyWorkbench draft={draft} onChange={setDraft} presets={withPresets ? presets : undefined} {...props} />
}

const editor = () => screen.findByLabelText("Rego policy editor") as Promise<HTMLTextAreaElement>
const markers = async () => JSON.parse((await editor()).getAttribute("data-markers") ?? "[]")

function chooseSample(label: string) {
  const select = createWrapper().findSelect()!
  select.openDropdown()
  select.selectOptionByValue(SAMPLES.find(s => s.label === label)!.id)
}
const callText = () => document.querySelector('textarea[aria-label="Sample call"]') as HTMLTextAreaElement
const runTest = () => fireEvent.click(screen.getByRole("button", { name: "Run test" }))

describe("policy workbench", () => {
  it("turns the server's errors[] into markers and a problems pane", async () => {
    startBackend()
    render(<Bench start={rego(BAD)} />)
    const problems = await screen.findByRole("list", { name: "Problems" })
    expect(problems.textContent).toContain("3:20  undefined function http.send")
    expect(screen.getByRole("status").textContent).toContain("1 error")
    expect(screen.getByRole("status").textContent).toContain("0 warnings")
    expect(await markers()).toEqual([
      expect.objectContaining({ startLineNumber: 3, startColumn: 20, endLineNumber: 3, endColumn: 29, severity: "error", code: "rego_type_error" }),
    ])
  })

  it("clears them once the policy is valid", async () => {
    startBackend()
    const verdicts: (Validation | null)[] = []
    render(<Bench start={rego(BAD)} onValidation={v => verdicts.push(v)} />)
    await screen.findByRole("list", { name: "Problems" })
    fireEvent.change(await editor(), { target: { value: presetSource("no-scripting") } })
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("Valid"))
    expect(screen.queryByRole("list", { name: "Problems" })).toBeNull()
    expect(await markers()).toEqual([])
    expect(verdicts.at(-1)).toMatchObject({ ok: true })
  })

  it("shows a warning that has no position in the problems pane, with no marker", async () => {
    startBackend()
    render(<Bench start={rego(WITH_SHELL)} />)
    const problems = await screen.findByRole("list", { name: "Problems" })
    expect(problems.textContent).toMatch(/^Warning this policy restricts what browser_execute may do and allows the shell/)
    const status = screen.getByRole("status").textContent
    expect(status).toContain("0 errors")
    expect(status).toContain("1 warning")
    expect(status).toContain("Valid") // a warning does not stop a save
    expect(await markers()).toEqual([])
  })

  it("marks what a refused save answered until the check has its own verdict", async () => {
    // A backend whose check cannot be reached: only the save's 422 is known.
    globalThis.__testFetch = async () => new Response(JSON.stringify({ error: "the operator could not be reached" }), { status: 503, headers: { "Content-Type": "application/json" } })
    const refused = { errors: [{ row: 2, col: 3, code: "rego_type_error", message: "undefined function http.send" }], warnings: [{ code: "w", message: "no place" }] }
    render(<Bench start={rego("package browserjs.policy\n  http.send({})\n")} refused={refused} />)
    const problems = await screen.findByRole("list", { name: "Problems" })
    expect(problems.textContent).toContain("2:3  undefined function http.send")
    expect(problems.textContent).toContain("Warning no place")
    expect(await markers()).toEqual([expect.objectContaining({ startLineNumber: 2, startColumn: 3, severity: "error" })])
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("Couldn't check the policy: the operator could not be reached"))
  })

  it("is one Rego editor: no format to choose, no second pane, no schema asked for", async () => {
    const { sent } = startBackend()
    render(<Bench start={rego(presetSource("no-scripting"))} />)
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("Valid"))
    expect(screen.getByRole("status").textContent).toContain("Rego")
    expect(screen.queryByRole("button", { name: "JSON" })).toBeNull()
    expect(screen.queryByRole("button", { name: "Rego" })).toBeNull()
    expect(screen.queryByLabelText(/Generated Rego/)).toBeNull()
    expect(document.body.textContent).not.toMatch(/JSON policy|policy\.json/)
    expect(sent.map(r => r.path)).toEqual(["/api/policies/validate"])
    expect(sent[0].body).toEqual({ kind: "rego", source: presetSource("no-scripting") })
  })

  it("loads a preset's Rego into the editor", async () => {
    startBackend()
    render(<Bench start={rego(BAD)} withPresets />)
    const dropdown = createWrapper().findButtonDropdown()!
    dropdown.openDropdown()
    expect(dropdown.findItems().map(i => i.getElement().textContent)).toEqual(presets.map(p => expect.stringContaining(p.title)))
    dropdown.findItemById("read-only-shell")!.click()
    expect((await editor()).value).toBe(presetSource("read-only-shell"))
    expect((await editor()).value).toContain("package browserjs.policy")
  })

  it("has no presets to offer unless it is given them", async () => {
    startBackend()
    render(<Bench start={rego(BAD)} />)
    await editor()
    expect(screen.queryByRole("button", { name: "Start from a preset" })).toBeNull()
  })

  it("tests the policy against a sample call", async () => {
    const { sent } = startBackend()
    render(<Bench start={rego(presetSource("one-site"))} />)
    chooseSample("Run git status")
    expect(callText().value).toContain('"cmd": "git status"')
    runTest()
    expect(await screen.findByText("Denied")).toBeTruthy()
    const call = sent.find(r => r.path === "/api/policies/evaluate")!
    expect(call.body).toEqual({
      kind: "rego",
      source: presetSource("one-site"),
      input: { operation: "mcp_call_tool", server: "exec", tool: "exec", arguments: { cmd: "git status", timeout: 30 } },
    })

    fireEvent.change(await editor(), { target: { value: presetSource("unrestricted") } })
    expect(screen.queryByText("Denied")).toBeNull() // a result belongs to the policy it was run with
    runTest()
    expect(await screen.findByText("Allowed")).toBeTruthy()

    // A tool the platform does not know is refused whatever the policy says.
    chooseSample("A tool that does not exist")
    expect(screen.queryByText("Allowed")).toBeNull()
    runTest()
    expect(await screen.findByText("Denied")).toBeTruthy()
  })

  it("offers the sample calls grouped by the tool they call", async () => {
    startBackend()
    render(<Bench start={rego(presetSource("unrestricted"))} />)
    const select = createWrapper().findSelect()!
    select.openDropdown()
    const groups = select.findDropdown().findGroups().map(g => g.getElement().textContent)
    expect(groups).toEqual(["browser / browser_execute", "browser / desktop_execute", "exec / exec", "exec / stream_logs", "exec / search_logs", "browser / file_write"])
    expect(select.findDropdown().findOptions()).toHaveLength(SAMPLES.length)
    select.selectOptionByValue("desktop-clipboard")
    expect(JSON.parse(callText().value)).toMatchObject({
      server: "browser",
      tool: "desktop_execute",
      arguments: { operations: [{ type: "clipboard.getContent" }] },
    })
  })

  it("read-only shows the source, with no editor, no presets and no check", async () => {
    const { sent } = startBackend()
    render(<Bench start={rego(presetSource("one-site"))} readOnly withPresets regoInForce={presetSource("one-site")} />)
    expect(screen.getByLabelText("policy.rego, read-only").textContent).toBe(presetSource("one-site"))
    expect(screen.queryByLabelText("Policy in force, read-only")).toBeNull() // the source is what is in force
    expect(screen.queryByLabelText("Rego policy editor")).toBeNull()
    expect(screen.queryByRole("button", { name: "Start from a preset" })).toBeNull()
    expect(screen.getByRole("button", { name: "Run test" })).toBeTruthy()
    await new Promise(r => setTimeout(r, 500))
    expect(sent).toEqual([])
  })

  it("read-only shows the policy in force beside a source that is not it", () => {
    startBackend()
    render(<Bench start={rego(BAD)} readOnly regoInForce={presetSource("one-site")} />)
    expect(screen.getByLabelText("policy.rego, read-only").textContent).toBe(BAD)
    expect(screen.getByLabelText("Policy in force, read-only").textContent).toBe(presetSource("one-site"))
  })
})

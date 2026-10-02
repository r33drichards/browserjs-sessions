// @vitest-environment jsdom
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react"
import { useState } from "react"
import { afterEach, describe, expect, it, vi } from "vitest"
import type { PolicySource, Validation } from "../policyApi"
import { FakeEditor, presetSource, startBackend } from "../test/harness"
import type { Problems } from "./PolicyWorkbench"
import { PolicyWorkbench } from "./PolicyWorkbench"

vi.mock("./MonacoEditor", () => ({ default: FakeEditor }))

afterEach(cleanup)

const BAD = '{\n  "version": 1,\n  "allow": {\n    "operations": ["clik"]\n  }\n}'

function Bench(props: { start: PolicySource; refused?: Problems; readOnly?: boolean; onValidation?: (v: Validation | null) => void }) {
  const [draft, setDraft] = useState(props.start)
  return <PolicyWorkbench draft={draft} onChange={setDraft} refused={props.refused} readOnly={props.readOnly} onValidation={props.onValidation} regoInForce="package browserjs.policy" />
}

const markers = async () => JSON.parse((await screen.findByLabelText(/policy editor$/)).getAttribute("data-markers") ?? "[]")

describe("policy workbench", () => {
  it("turns the server's errors[] into markers and a problems pane", async () => {
    startBackend()
    render(<Bench start={{ kind: "json", source: BAD }} />)
    const problems = await screen.findByRole("list", { name: "Problems" })
    expect(problems.textContent).toContain('4:20  unknown operation "clik"; did you mean "click"?')
    expect(screen.getByRole("status").textContent).toContain("1 error")
    expect(screen.getByRole("status").textContent).toContain("0 warnings")
    expect(await markers()).toEqual([
      expect.objectContaining({ startLineNumber: 4, startColumn: 20, endLineNumber: 4, endColumn: 26, severity: "error", code: "unknown_operation" }),
    ])
    // No generated Rego for a policy that does not validate.
    expect(screen.getByLabelText("Generated Rego, read-only").textContent).toBe("Shown when the JSON policy is valid.")
  })

  it("clears them, and shows the generated Rego, once the policy is valid", async () => {
    startBackend()
    const verdicts: (Validation | null)[] = []
    render(<Bench start={{ kind: "json", source: BAD }} onValidation={v => verdicts.push(v)} />)
    await screen.findByRole("list", { name: "Problems" })
    fireEvent.change(screen.getByLabelText("JSON policy editor"), { target: { value: presetSource("no-scripting") } })
    await waitFor(() => expect(screen.getByLabelText("Generated Rego, read-only").textContent).toContain("package browserjs.policy"))
    expect(screen.queryByRole("list", { name: "Problems" })).toBeNull()
    expect(await markers()).toEqual([])
    expect(screen.getByRole("status").textContent).toContain("Valid")
    expect(verdicts.at(-1)).toMatchObject({ ok: true })
  })

  it("marks what a refused save answered until the check has its own verdict", async () => {
    // A backend whose check cannot be reached: only the save's 422 is known.
    globalThis.__testFetch = async () => new Response(JSON.stringify({ error: "the operator could not be reached" }), { status: 503, headers: { "Content-Type": "application/json" } })
    const refused = { errors: [{ row: 2, col: 3, code: "rego_type_error", message: "undefined function http.send" }], warnings: [{ code: "w", message: "no place" }] }
    render(<Bench start={{ kind: "rego", source: "package browserjs.policy\n  http.send({})\n" }} refused={refused} />)
    expect((await screen.findByRole("list", { name: "Problems" })).textContent).toContain("2:3  undefined function http.send")
    expect(await markers()).toEqual([
      expect.objectContaining({ startLineNumber: 2, startColumn: 3, severity: "error" }),
      expect.objectContaining({ startLineNumber: 1, startColumn: 1, severity: "warning" }),
    ])
    await waitFor(() => expect(screen.getByRole("status").textContent).toContain("Couldn't check the policy: the operator could not be reached"))
  })

  it("converts JSON to its generated Rego after a confirmation", async () => {
    startBackend()
    render(<Bench start={{ kind: "json", source: presetSource("no-scripting") }} />)
    await waitFor(() => expect(screen.getByLabelText("Generated Rego, read-only").textContent).toContain("package"))
    fireEvent.click(screen.getByRole("button", { name: "Rego" }))
    expect(screen.getByLabelText("JSON policy editor")).toBeTruthy() // not yet
    fireEvent.click(screen.getByRole("button", { name: "Convert to Rego" }))
    const editor = (await screen.findByLabelText("Rego policy editor")) as HTMLTextAreaElement
    expect(editor.value).toContain("package browserjs.policy")
    expect(screen.queryByLabelText("Generated Rego, read-only")).toBeNull() // in Rego there is one pane
  })

  it("tests the policy against a sample call", async () => {
    const { sent } = startBackend()
    render(<Bench start={{ kind: "json", source: presetSource("observe-only") }} />)
    fireEvent.click(screen.getByRole("button", { name: "Run test" }))
    expect(await screen.findByText("Denied")).toBeTruthy()
    const call = sent.find(r => r.path === "/api/policies/evaluate")!
    expect(call.body).toMatchObject({ kind: "json", source: presetSource("observe-only"), input: { tool: "browser_execute" } })

    fireEvent.change(screen.getByLabelText("JSON policy editor"), { target: { value: presetSource("unrestricted") } })
    expect(screen.queryByText("Denied")).toBeNull() // a result belongs to the policy it was run with
    fireEvent.click(screen.getByRole("button", { name: "Run test" }))
    expect(await screen.findByText("Allowed")).toBeTruthy()
  })

  it("read-only shows the source and the Rego in force, with no editor and no check", async () => {
    const { sent } = startBackend()
    render(<Bench start={{ kind: "json", source: presetSource("one-site") }} readOnly />)
    expect(screen.getByLabelText("policy.json, read-only").textContent).toBe(presetSource("one-site"))
    expect(screen.getByLabelText("Generated Rego, read-only").textContent).toBe("package browserjs.policy")
    expect(screen.queryByLabelText("JSON policy editor")).toBeNull()
    expect(screen.queryByRole("button", { name: "Rego" })).toBeNull()
    expect(screen.getByRole("button", { name: "Run test" })).toBeTruthy()
    await new Promise(r => setTimeout(r, 500))
    expect(sent.some(r => r.path === "/api/policies/validate")).toBe(false)
  })
})

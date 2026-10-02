import { describe, expect, it } from "vitest"
import { problemLine, toMarker, toMarkers } from "./markers"
import { REGO_TEMPLATE, regoKeywords, regoMonarch } from "./rego"

const source = 'package browserjs.policy\n\nallow_tool_call if {\n\tinput.tool == "browser_execute"\n}\n'

describe("markers", () => {
  it("ends a marker at the end of the token the server pointed at", () => {
    const m = toMarker({ row: 4, col: 16, code: "rego_type_error", message: "match error" }, source, "error")
    expect(m).toMatchObject({ startLineNumber: 4, startColumn: 16, endLineNumber: 4, endColumn: 33, severity: "error" })
    expect(source.split("\n")[3].slice(m.startColumn - 1, m.endColumn - 1)).toBe('"browser_execute"')
  })

  it("covers the line when there is no column", () => {
    expect(toMarker({ row: 3, code: "c", message: "m" }, source, "warning")).toMatchObject({ startLineNumber: 3, startColumn: 1, endColumn: 21 })
  })

  it("gives a diagnostic without a place no marker", () => {
    const bypass = { code: "browser_bypass_shell", message: "the shell walks around this" }
    expect(toMarkers([{ code: "e", message: "nowhere" }], [bypass, { row: 2, col: 1, code: "w", message: "here" }], source)).toEqual([
      expect.objectContaining({ startLineNumber: 2, severity: "warning", code: "w" }),
    ])
  })

  it("keeps a position past the end of the source inside it", () => {
    const m = toMarker({ row: 99, col: 99, code: "c", message: "m" }, "a\nb", "error")
    expect(m.startLineNumber).toBe(2)
    expect(m.endColumn).toBeGreaterThan(m.startColumn)
  })

  it("puts errors before warnings and formats a problem line", () => {
    const all = toMarkers([{ row: 1, col: 1, code: "e", message: "bad" }], [{ row: 2, col: 1, code: "w", message: "meh" }], source)
    expect(all.map(m => m.severity)).toEqual(["error", "warning"])
    expect(problemLine({ row: 4, col: 21, message: "undefined function" })).toBe("4:21  undefined function")
    expect(problemLine({ message: "no place" })).toBe("no place")
  })
})

describe("rego grammar", () => {
  it("has the v1 keywords and rules that compile", () => {
    for (const word of ["package", "import", "if", "contains", "every", "in", "some", "not", "default", "else", "with", "as"])
      expect(regoKeywords).toContain(word)
    for (const state of Object.values(regoMonarch.tokenizer)) {
      for (const rule of state) expect(rule[0]).toBeInstanceOf(RegExp)
    }
  })

  it("matches comments, strings and names the way Rego writes them", () => {
    const [comment] = regoMonarch.tokenizer.root[0] as [RegExp, string]
    expect(comment.test("# allow.rules[0]")).toBe(true)
    const [call] = regoMonarch.tokenizer.root[1] as [RegExp, unknown]
    expect(call.exec("regex.match(x)")?.[0]).toBe("regex.match")
    expect(call.exec("allow_tool_call if {")).toBeNull()
  })

  it("starts a new policy from a module that says what is refused and what can be walked around", () => {
    expect(REGO_TEMPLATE).toMatch(/^package browserjs\.policy$/m)
    expect(REGO_TEMPLATE).toContain("allow_tool_call if {")
    expect(REGO_TEMPLATE).toMatch(/desktop_execute and the exec server's tools[^]*refused until a rule here allows them/)
    expect(REGO_TEMPLATE).toMatch(/can be\n# walked around/)
  })
})

import { describe, expect, it } from "vitest"
import { mergeProblems, problemLine, toMarker, toMarkers } from "./markers"
import { regoKeywords, regoMonarch } from "./rego"

const source = '{\n  "version": 1,\n  "allow": {\n    "operations": ["clik"]\n  }\n}\n'

describe("markers", () => {
  it("ends a marker at the end of the token the server pointed at", () => {
    const m = toMarker({ row: 4, col: 20, code: "unknown_operation", message: "unknown operation" }, source, "error")
    expect(m).toMatchObject({ startLineNumber: 4, startColumn: 20, endLineNumber: 4, endColumn: 26, severity: "error" })
    expect(source.split("\n")[3].slice(m.startColumn - 1, m.endColumn - 1)).toBe('"clik"')
  })

  it("covers the line when there is no column, and the first line when there is no place", () => {
    expect(toMarker({ row: 2, code: "c", message: "m" }, source, "warning")).toMatchObject({ startLineNumber: 2, startColumn: 1, endColumn: 16 })
    expect(toMarker({ code: "c", message: "m" }, source, "error")).toMatchObject({ startLineNumber: 1, startColumn: 1, endColumn: 2 })
  })

  it("keeps a position past the end of the source inside it", () => {
    const m = toMarker({ row: 99, col: 99, code: "c", message: "m" }, "a\nb", "error")
    expect(m.startLineNumber).toBe(2)
    expect(m.endColumn).toBeGreaterThan(m.startColumn)
  })

  it("puts errors before warnings and formats a problem line", () => {
    const all = toMarkers([{ row: 1, col: 1, code: "e", message: "bad" }], [{ row: 2, col: 1, code: "w", message: "meh" }], source)
    expect(all.map(m => m.severity)).toEqual(["error", "warning"])
    expect(problemLine({ row: 4, col: 21, message: "unknown operation" })).toBe("4:21  unknown operation")
    expect(problemLine({ message: "no place" })).toBe("no place")
  })

  it("does not repeat a local problem the server reports at the same place", () => {
    const server = [{ row: 4, col: 20, code: "s", message: "server" }]
    const local = [
      { row: 4, col: 20, code: "json_error", message: "local, same place" },
      { row: 1, col: 1, code: "json_error", message: "local only" },
    ]
    expect(mergeProblems(server, local).map(d => d.message)).toEqual(["server", "local only"])
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
})

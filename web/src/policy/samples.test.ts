import { describe, expect, it } from "vitest"
import { presetsFromExamples } from "../../mock/backend"
import { SAMPLES, SAMPLE_GROUPS, sampleText } from "./samples"

const examples = import.meta.glob("../../../docs/contracts/policy/examples/*", { query: "?raw", import: "default", eager: true }) as Record<string, string>
const cases = presetsFromExamples(examples).flatMap(p => p.cases)

const byLabel = (label: string) => SAMPLES.find(s => s.label === label)!.input as any

describe("sample calls", () => {
  it("are grouped by the server and tool they call", () => {
    expect(SAMPLE_GROUPS.map(g => [g.label, g.samples.length])).toEqual([
      ["browser / browser_execute", 5],
      ["browser / desktop_execute", 4],
      ["exec / exec", 2],
      ["exec / stream_logs", 1],
      ["exec / search_logs", 1],
      ["browser / file_write", 1],
    ])
    expect(SAMPLE_GROUPS.flatMap(g => g.samples)).toEqual(SAMPLES)
    expect(new Set(SAMPLES.map(s => s.id)).size).toBe(SAMPLES.length)
  })

  it("are the input a policy is asked about", () => {
    for (const s of SAMPLES) {
      expect(s.input).toMatchObject({ operation: "mcp_call_tool", server: s.server, tool: s.tool })
      expect(JSON.parse(sampleText(s))).toEqual(s.input)
    }
  })

  it("cover the desktop and the shell", () => {
    expect(SAMPLES.filter(s => s.tool === "desktop_execute").map(s => s.label)).toEqual([
      "Take a screenshot of the desktop",
      "Click and type on the desktop",
      "Press a key combination",
      "Read the clipboard",
    ])
    expect(byLabel("Read the clipboard").arguments.operations).toEqual([{ type: "clipboard.getContent" }])
    expect(byLabel("Run git status").arguments).toEqual({ cmd: "git status", timeout: 30 })
    expect(byLabel("Run a command that talks to the browser's debugging port").arguments).toEqual({
      cmd: "curl -s http://127.0.0.1:9222/json/version",
      timeout: 30,
    })
    expect(byLabel("Read a command's output")).toMatchObject({ server: "exec", tool: "stream_logs", arguments: { offset: 0 } })
    expect(byLabel("Search a command's output")).toMatchObject({ server: "exec", tool: "search_logs", arguments: { pattern: "error" } })
    expect(byLabel("A tool that does not exist")).toMatchObject({ server: "browser", tool: "file_write" })
  })

  // So that the mock backend, which cannot run Rego, answers these from the
  // contract's cases rather than from its placeholder.
  it("are, for the desktop and the shell, calls the contract's examples have cases for", () => {
    const known = new Set(cases.map(c => JSON.stringify(c.input)))
    const outside = SAMPLES.filter(s => s.tool !== "browser_execute" && !known.has(JSON.stringify(s.input))).map(s => s.label)
    expect(outside).toEqual(["Read the clipboard"])
  })
})

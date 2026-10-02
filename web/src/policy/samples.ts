// Sample calls for the editor's Test: mcp_tools inputs as mcp-js sends them
// (docs/contracts/policy/input-sample.json is the first).
type Operation = { type: string; params: Record<string, unknown> }

const call = (operations: Operation[]) => ({
  operation: "mcp_call_tool",
  server: "browser",
  tool: "browser_execute",
  arguments: { operations, tab: "default" },
})

export interface Sample {
  id: string
  label: string
  input: unknown
}

export const SAMPLES: Sample[] = [
  {
    id: "sign-in",
    label: "Sign in on a page",
    input: call([
      { type: "navigate", params: { url: "https://example.com/login" } },
      { type: "type", params: { selector: "#user", text: "ada" } },
      { type: "press", params: { key: "Enter" } },
      { type: "wait", params: { selector: "#home", ms: 5000 } },
      { type: "screenshot", params: {} },
    ]),
  },
  { id: "navigate", label: "Navigate to a URL", input: call([{ type: "navigate", params: { url: "https://example.com/" } }]) },
  { id: "screenshot", label: "Take a screenshot", input: call([{ type: "screenshot", params: {} }]) },
  {
    id: "evaluate",
    label: "Run script in the page",
    input: call([{ type: "evaluate", params: { script: "document.title" } }]),
  },
  {
    id: "other-site",
    label: "Navigate to another site",
    input: call([{ type: "navigate", params: { url: "https://other.example.org/" } }]),
  },
]

export const sampleText = (sample: Sample) => JSON.stringify(sample.input, null, 2)

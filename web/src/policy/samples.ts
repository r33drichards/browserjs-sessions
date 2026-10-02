// Sample calls for the editor's Test: the input a policy is asked about, one
// per tool call, as mcp-js sends it (docs/contracts/policy/input-sample.json
// is the first). Grouped by the tool they call.
type Operation = { type: string; params?: Record<string, unknown> }

export interface Sample {
  id: string
  label: string
  server: string
  tool: string
  input: unknown
}

const sample = (id: string, label: string, server: string, tool: string, args: unknown): Sample => ({
  id,
  label,
  server,
  tool,
  input: { operation: "mcp_call_tool", server, tool, arguments: args },
})

const browser = (id: string, label: string, operations: Operation[]) =>
  sample(id, label, "browser", "browser_execute", { operations, tab: "default" })
const desktop = (id: string, label: string, operations: Operation[]) =>
  sample(id, label, "browser", "desktop_execute", { operations })

// The id exec answers with, which the two log tools take.
const COMMAND_ID = "3f0e8a52-5d2b-4a57-9f3b-0c1d2e3f4a5b"

export const SAMPLES: Sample[] = [
  browser("sign-in", "Sign in on a page", [
    { type: "navigate", params: { url: "https://example.com/login" } },
    { type: "type", params: { selector: "#user", text: "ada" } },
    { type: "press", params: { key: "Enter" } },
    { type: "wait", params: { selector: "#home", ms: 5000 } },
    { type: "screenshot", params: {} },
  ]),
  browser("navigate", "Navigate to a URL", [{ type: "navigate", params: { url: "https://example.com/" } }]),
  browser("screenshot", "Take a screenshot", [{ type: "screenshot", params: {} }]),
  browser("evaluate", "Run script in the page", [{ type: "evaluate", params: { script: "document.title" } }]),
  browser("other-site", "Navigate to another site", [{ type: "navigate", params: { url: "https://other.example.org/" } }]),

  desktop("desktop-screenshot", "Take a screenshot of the desktop", [{ type: "screen.grab" }]),
  // The address bar: what makes desktop control a way around a browser rule.
  desktop("desktop-type", "Click and type on the desktop", [
    { type: "mouse.click", params: { x: 640, y: 52 } },
    { type: "keyboard.type", params: { text: "javascript:alert(1)" } },
    { type: "keyboard.type", params: { keys: ["Enter"] } },
  ]),
  desktop("desktop-keys", "Press a key combination", [
    { type: "keyboard.pressKey", params: { keys: ["LeftControl", "LeftShift", "J"] } },
  ]),
  desktop("desktop-clipboard", "Read the clipboard", [{ type: "clipboard.getContent" }]),

  sample("shell-git-status", "Run git status", "exec", "exec", { bin: "git", args: ["status"], timeout: 30 }),
  sample("shell-debug-port", "Run a program that talks to the browser's debugging port", "exec", "exec", {
    bin: "curl",
    args: ["-s", "http://127.0.0.1:9222/json/version"],
    timeout: 30,
  }),
  sample("shell-sh", "Run a shell", "exec", "exec", { bin: "sh", args: ["-c", "id"], timeout: 30 }),
  sample("shell-stream-logs", "Read a command's output", "exec", "stream_logs", { id: COMMAND_ID, offset: 0 }),
  sample("shell-search-logs", "Search a command's output", "exec", "search_logs", { id: COMMAND_ID, pattern: "error" }),
  sample("shell-kill", "Stop a command", "exec", "kill", { id: COMMAND_ID }),

  // Refused whatever the policy says: the platform does not know the tool.
  sample("unknown-tool", "A tool that does not exist", "browser", "file_write", { operations: [] }),
]

export interface SampleGroup {
  label: string // "browser / browser_execute"
  samples: Sample[]
}

// The samples by the tool they call, in the order above.
export const SAMPLE_GROUPS: SampleGroup[] = SAMPLES.reduce<SampleGroup[]>((groups, s) => {
  const label = `${s.server} / ${s.tool}`
  const group = groups.find(g => g.label === label)
  if (group) group.samples.push(s)
  else groups.push({ label, samples: [s] })
  return groups
}, [])

export const sampleText = (s: Sample) => JSON.stringify(s.input, null, 2)

// Renders a page against the mock backend (mock/backend.ts), which answers as
// docs/contracts/policy/backend-api.yaml says, with the contract's examples.
import { render } from "@testing-library/react"
import { RouterProvider, createMemoryRouter, useLocation } from "react-router-dom"
import type { RouteObject } from "react-router-dom"
import type { MockOptions } from "../../mock/backend"
import { createMockBackend, presetsFromExamples } from "../../mock/backend"
import { MeProvider } from "../auth/MeProvider"
import { forgetTokensProbe } from "../policyApi"

const examples = import.meta.glob("../../../docs/contracts/policy/examples/*", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>
const schema = import.meta.glob("../../../docs/contracts/policy/json-policy.schema.json", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>

export const presets = presetsFromExamples(examples)
export const presetSource = (id: string) => presets.find(p => p.id === id)!.source

export interface Sent {
  method: string
  path: string
  body: any
  headers: Headers
}

export function startBackend(options: Partial<MockOptions> = {}) {
  const backend = createMockBackend({ presets, schema: JSON.parse(Object.values(schema)[0]), ...options })
  const sent: Sent[] = []
  forgetTokensProbe()
  globalThis.__testFetch = async (input, init) => {
    sent.push({
      method: init?.method ?? "GET",
      path: String(input),
      body: typeof init?.body === "string" ? JSON.parse(init.body) : undefined,
      headers: new Headers(init?.headers),
    })
    return backend.fetch(input, init)
  }
  const sessionNamed = (name: string) => [...backend.sessions.values()].find(s => s.name === name)!
  return { backend, sent, sessionNamed, writes: () => sent.filter(r => r.method !== "GET" && !r.path.startsWith("/api/policies/")) }
}

// Where a flow ended up: the path, and the message it carried there.
export function Landed() {
  const location = useLocation()
  const flash = (location.state as { flash?: { content: string } } | null)?.flash
  return (
    <div data-testid="landed">
      {location.pathname}
      {location.search} {flash?.content}
    </div>
  )
}

export function renderAt(path: string, routes: RouteObject[]) {
  const router = createMemoryRouter([...routes, { path: "*", element: <Landed /> }], { initialEntries: [path] })
  return {
    router,
    ...render(
      <MeProvider>
        <RouterProvider router={router} />
      </MeProvider>,
    ),
  }
}

// In place of Monaco, which needs a real browser: a textarea that shows what
// the editor was given. Use with vi.mock("…/components/MonacoEditor", …).
export function FakeEditor(props: { value: string; onChange: (v: string) => void; markers: unknown[]; ariaLabel: string; kind: string }) {
  return (
    <textarea
      aria-label={props.ariaLabel}
      data-kind={props.kind}
      data-markers={JSON.stringify(props.markers)}
      value={props.value}
      onChange={e => props.onChange(e.target.value)}
    />
  )
}

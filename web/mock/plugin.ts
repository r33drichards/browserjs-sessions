// `npm run dev:mock`: the dev server answers /api itself, from mock/backend.ts,
// with the contract's own examples and schema. No backend, cluster or proxy.
//   MOCK=1          every feature on
//   MOCK=off        a backend with policies and tokens off (today's API)
import { readFileSync, readdirSync } from "node:fs"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"
import type { Plugin } from "vite"
import { createMockBackend, presetsFromExamples } from "./backend"

const contracts = join(dirname(fileURLToPath(import.meta.url)), "../../docs/contracts/policy")

export function mockBackend(mode: string): Plugin {
  const examples = join(contracts, "examples")
  const files = Object.fromEntries(readdirSync(examples).map(name => [name, readFileSync(join(examples, name), "utf8")]))
  const on = mode !== "off"
  const backend = createMockBackend({
    presets: presetsFromExamples(files),
    schema: JSON.parse(readFileSync(join(contracts, "json-policy.schema.json"), "utf8")),
    policies: on,
    tokens: on,
  })
  return {
    name: "browserjs-mock-backend",
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        if (req.url === "/config.js") {
          res.setHeader("Content-Type", "application/javascript")
          return res.end("window.__BROWSERJS_CFG__ = {};")
        }
        if (!req.url?.startsWith("/api")) return next()
        const chunks: Buffer[] = []
        req.on("data", chunk => chunks.push(chunk))
        req.on("end", () => {
          const raw = Buffer.concat(chunks).toString("utf8")
          const headers: Record<string, string> = {}
          for (const [name, value] of Object.entries(req.headers)) if (typeof value === "string") headers[name] = value
          const answer = backend.handle({ method: req.method ?? "GET", path: req.url!, headers, body: raw ? JSON.parse(raw) : undefined })
          res.statusCode = answer.status
          res.setHeader("Content-Type", "application/json")
          for (const [name, value] of Object.entries(answer.headers ?? {})) res.setHeader(name, value)
          res.end(answer.text ?? (answer.body === undefined ? "" : JSON.stringify(answer.body)))
        })
      })
    },
  }
}

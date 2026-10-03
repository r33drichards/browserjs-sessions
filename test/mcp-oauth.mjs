// Walks Pomerium's MCP sign-in by hand, as Claude Code's client would, on
// the local cluster:
//
//   node test/mcp-oauth.mjs https://sessions.localtest.me/<id>/mcp alice@example.com
//
// The first argument is the session's MCP URL, as the UI shows it. (A bare
// host, <id>.sessions.localtest.me, is taken for https://<host>/mcp: the
// host a session had to itself before.)
//
// discovery -> authorize (PKCE, Claude Code's client ID document, loopback
// redirect) -> sign in at Dex in a headless Chrome -> token -> MCP
// initialize, tools/list, run_js. Prints what each step answered.
//
// test/mcp-client.mjs does the same with a real client; this one shows each
// step. SKIP_DISCOVERY=1 supplies the endpoints instead of discovering them.
import { createRequire } from "node:module"
import { dirname, resolve } from "node:path"
import { fileURLToPath } from "node:url"
import crypto from "node:crypto"
const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..")
const require = createRequire(ROOT + "/computer-use-mcp/browser/")
const puppeteer = require("puppeteer-core")
process.env.NODE_TLS_REJECT_UNAUTHORIZED = "0"
const [target, email] = process.argv.slice(2)
const MCP = target.includes("://") ? target : `https://${target}/mcp`
const ORIGIN = new URL(MCP).origin
const CLIENT = "https://claude.ai/oauth/claude-code-client-metadata"
const REDIRECT = "http://localhost:53682/callback"
const out = {}
const post = (body, token, sid) => fetch(MCP, { method: "POST", headers: { "content-type": "application/json", accept: "application/json, text/event-stream", ...(token ? { authorization: "Bearer " + token } : {}), ...(sid ? { "mcp-session-id": sid } : {}) }, body: JSON.stringify(body) })
const parse = async r => { const t = await r.text(); const d = t.split("\n").filter(l => l.startsWith("data:") && l.length > 6).pop(); return d ? JSON.parse(d.slice(5)) : (t.startsWith("{") ? JSON.parse(t) : t.slice(0, 200)) }

let r, meta
if (process.env.SKIP_DISCOVERY) {
  meta = { authorization_endpoint: `${ORIGIN}/.pomerium/mcp/authorize`, token_endpoint: `${ORIGIN}/.pomerium/mcp/token` }
  out.protected_resource = { body: { resource: MCP } }
  out.discovery = "skipped: endpoints supplied by hand"
} else {
  r = await post({ jsonrpc: "2.0", id: 1, method: "initialize", params: {} })
  out.unauthenticated = { status: r.status, www_authenticate: r.headers.get("www-authenticate") }
  const prmURL = /resource_metadata="([^"]+)"/.exec(out.unauthenticated.www_authenticate || "")?.[1]
  r = await fetch(prmURL); out.protected_resource = { status: r.status, body: r.status === 200 ? await r.json() : (await r.text()).slice(0, 80) }
  if (r.status !== 200) { console.log(JSON.stringify(out, null, 1)); process.exit(2) }
  const as = out.protected_resource.body.authorization_servers[0]
  r = await fetch(as + "/.well-known/oauth-authorization-server"); meta = await r.json(); out.authorization_server = { status: r.status, issuer: meta.issuer, registration_endpoint: meta.registration_endpoint ?? null, cimd: meta.client_id_metadata_document_supported }
}

const verifier = crypto.randomBytes(32).toString("base64url")
const challenge = crypto.createHash("sha256").update(verifier).digest("base64url")
const state = crypto.randomBytes(8).toString("hex")
const authURL = meta.authorization_endpoint + "?" + new URLSearchParams({ response_type: "code", client_id: CLIENT, redirect_uri: REDIRECT, code_challenge: challenge, code_challenge_method: "S256", state, resource: out.protected_resource.body.resource })

const browser = await puppeteer.launch({ executablePath: process.env.CHROME ?? "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", headless: true, args: ["--ignore-certificate-errors"] })
const page = await browser.newPage()
let callback = null
await page.setRequestInterception(true)
page.on("request", q => { if (q.url().startsWith(REDIRECT)) { callback = q.url(); q.respond({ status: 200, body: "ok" }) } else q.continue() })
const hops = []
page.on("framenavigated", f => { if (f === page.mainFrame()) hops.push(f.url().split("?")[0]) })
await page.goto(authURL, { waitUntil: "networkidle2" })
if (page.url().startsWith("http://localhost:5556/dex/auth")) {
  await Promise.all([page.waitForNavigation({ waitUntil: "networkidle2" }), page.click('a[href^="/dex/auth/local"]')])
  await page.type('input[name="login"]', email); await page.type('input[name="password"]', "test")
  await Promise.all([page.waitForNavigation({ waitUntil: "networkidle2" }).catch(() => {}), page.click('button[type="submit"]')])
}
for (let i = 0; i < 40 && !callback; i++) await new Promise(r => setTimeout(r, 250))
out.authorize = { hops: [...new Set(hops)], callback: callback ? callback.replace(/code=[^&]+/, "code=…") : null, page: callback ? undefined : (await page.evaluate(() => document.body.innerText)).slice(0, 300) }
await browser.close()
if (!callback) { console.log(JSON.stringify(out, null, 1)); process.exit(3) }
const cb = new URL(callback)
out.authorize.state_matches = cb.searchParams.get("state") === state
r = await fetch(meta.token_endpoint, { method: "POST", headers: { "content-type": "application/x-www-form-urlencoded" }, body: new URLSearchParams({ grant_type: "authorization_code", code: cb.searchParams.get("code"), redirect_uri: REDIRECT, client_id: CLIENT, code_verifier: verifier, resource: out.protected_resource.body.resource }) })
const tok = await r.json().catch(() => ({}))
out.token = { status: r.status, token_type: tok.token_type, expires_in: tok.expires_in, has_access: !!tok.access_token, has_refresh: !!tok.refresh_token, error: tok.error, error_description: tok.error_description }
if (!tok.access_token) { console.log(JSON.stringify(out, null, 1)); process.exit(4) }
if (process.env.TOKEN_FILE) require("node:fs").writeFileSync(process.env.TOKEN_FILE, tok.access_token, { mode: 0o600 })

r = await post({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-03-26", capabilities: {}, clientInfo: { name: "oauth-walk", version: "0" } } }, tok.access_token)
const sid = r.headers.get("mcp-session-id")
const init = await parse(r)
out.initialize = { status: r.status, serverInfo: init?.result?.serverInfo, body: r.status === 200 ? undefined : init }
if (r.status === 200) {
  await post({ jsonrpc: "2.0", method: "notifications/initialized" }, tok.access_token, sid)
  r = await post({ jsonrpc: "2.0", id: 2, method: "tools/list" }, tok.access_token, sid)
  out.tools = { status: r.status, names: (await parse(r))?.result?.tools?.map(t => t.name) }
  r = await post({ jsonrpc: "2.0", id: 3, method: "tools/call", params: { name: "run_js", arguments: { code: "console.log('via pomerium', 6*7)" } } }, tok.access_token, sid)
  out.run_js = { status: r.status, text: (await parse(r))?.result?.content?.[0]?.text }
}
console.log(JSON.stringify(out, null, 1))

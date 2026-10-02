// A real MCP client against a session through Pomerium: the official MCP
// SDK's client and its OAuth support do the discovery, the authorization
// request, the token exchange and the MCP calls. The only thing done for it
// is the user's part, signing in at Dex, in a headless Chrome.
//
//   node test/mcp-client.mjs https://sessions.localtest.me/<id>/mcp alice@example.com
//
// The first argument is the session's MCP URL, as the UI shows it. (A bare
// host, <id>.sessions.localtest.me, is taken for https://<host>/mcp: the
// host a session had to itself before.)
//
// The client identifies itself as Claude Code does (its client ID metadata
// document), which is what Pomerium is configured to accept. Exit status 0
// if the user could use the session, 1 otherwise.
import { createRequire } from "node:module"
import { dirname, resolve } from "node:path"
import { fileURLToPath, pathToFileURL } from "node:url"
const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..")
const modules = ROOT + "/images/browser/browser/node_modules/"
const sdk = name => import(pathToFileURL(modules + "@modelcontextprotocol/sdk/dist/esm/" + name).href)
const { Client } = await sdk("client/index.js")
const { StreamableHTTPClientTransport } = await sdk("client/streamableHttp.js")
const { UnauthorizedError } = await sdk("client/auth.js")
const puppeteer = createRequire(modules)("puppeteer-core")
process.env.NODE_TLS_REJECT_UNAUTHORIZED = "0" // the local CA

const [target, email] = process.argv.slice(2)
const url = new URL(target.includes("://") ? target : `https://${target}/mcp`)
const REDIRECT = "http://localhost:53682/callback"
const seen = []
const realFetch = globalThis.fetch
// What the client asked for on its own, for the record.
globalThis.fetch = async (input, init) => {
  const res = await realFetch(input, init)
  const u = new URL(typeof input === "string" || input instanceof URL ? input : input.url)
  seen.push(`${init?.method ?? "GET"} ${u.pathname} ${res.status}`)
  return res
}

// The user: opens the authorization URL, signs in at Dex, and ends up on the
// client's redirect URI with a code.
async function signIn(authorizationUrl) {
  const browser = await puppeteer.launch({ executablePath: process.env.CHROME ?? "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", headless: true, args: ["--ignore-certificate-errors"] })
  try {
    const page = await browser.newPage()
    let callback = null
    await page.setRequestInterception(true)
    page.on("request", q => { if (q.url().startsWith(REDIRECT)) { callback = q.url(); q.respond({ status: 200, body: "ok" }) } else q.continue() })
    await page.goto(String(authorizationUrl), { waitUntil: "networkidle2" })
    await Promise.all([page.waitForNavigation({ waitUntil: "networkidle2" }), page.click('a[href^="/dex/auth/local"]')])
    await page.type('input[name="login"]', email)
    await page.type('input[name="password"]', "test")
    await Promise.all([page.waitForNavigation({ waitUntil: "networkidle2" }).catch(() => {}), page.click('button[type="submit"]')])
    for (let i = 0; i < 40 && !callback; i++) await new Promise(r => setTimeout(r, 250))
    if (!callback) throw new Error("sign-in did not come back to the client: " + page.url())
    return new URL(callback).searchParams.get("code")
  } finally {
    await browser.close()
  }
}

let authorizationUrl, tokens, verifier, client
const provider = {
  get redirectUrl() { return REDIRECT },
  clientMetadataUrl: "https://claude.ai/oauth/claude-code-client-metadata",
  get clientMetadata() { return { client_name: "Claude Code", redirect_uris: [REDIRECT], grant_types: ["authorization_code", "refresh_token"], response_types: ["code"], token_endpoint_auth_method: "none" } },
  clientInformation: () => client,
  saveClientInformation: c => { client = c },
  tokens: () => tokens,
  saveTokens: t => { tokens = t },
  redirectToAuthorization: u => { authorizationUrl = u },
  saveCodeVerifier: v => { verifier = v },
  codeVerifier: () => verifier,
}

const out = { url: url.href, user: email }
let ok = false
try {
  let transport = new StreamableHTTPClientTransport(url, { authProvider: provider })
  let mcp = new Client({ name: "browserjs-mcp-client-test", version: "0" })
  try {
    await mcp.connect(transport)
    throw new Error("connected without signing in")
  } catch (e) {
    if (!(e instanceof UnauthorizedError)) throw e
  }
  out.authorization_endpoint = authorizationUrl.origin + authorizationUrl.pathname
  out.client_id = authorizationUrl.searchParams.get("client_id")
  await transport.finishAuth(await signIn(authorizationUrl))
  out.token = { type: tokens.token_type, expires_in: tokens.expires_in, refresh: !!tokens.refresh_token }

  transport = new StreamableHTTPClientTransport(url, { authProvider: provider })
  mcp = new Client({ name: "browserjs-mcp-client-test", version: "0" })
  await mcp.connect(transport)
  out.server = mcp.getServerVersion()
  out.tools = (await mcp.listTools()).tools.map(t => t.name)
  out.run_js = (await mcp.callTool({ name: "run_js", arguments: { code: "console.log('through pomerium', 6 * 7)" } })).content[0].text
  ok = out.tools.includes("run_js") && out.run_js.includes("through pomerium 42")
  await mcp.close()
} catch (e) {
  out.error = String(e.message ?? e).slice(0, 300)
}
out.requests = seen
console.log(JSON.stringify(out, null, 1))
process.exit(ok ? 0 : 1)

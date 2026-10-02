// The UI through Pomerium and Dex, in a headless Chrome, as the test user
// alice@example.com on the local cluster (hack/local-up.sh):
//
//   (cd images/browser/browser && npm ci)    # once, for puppeteer-core
//   node test/browser-e2e.mjs
//
// Signs in, creates a session, checks the live screen, loses the Pomerium
// session, signs out, tries a user who is not allowed in, and runs an MCP
// client against the session. Screenshots and browser-results.json go to OUT_DIR
// (default .local/). CHROME names the browser binary; KEEP=1 leaves the
// session in place.
import { createRequire } from "node:module"
import { dirname, resolve } from "node:path"
import { fileURLToPath } from "node:url"
const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), "..")
const OUT = process.env.OUT_DIR ?? ROOT + "/.local"
const CHROME = process.env.CHROME ?? "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
const require = createRequire(ROOT + "/images/browser/browser/")
const puppeteer = require("puppeteer-core")
const APP = "https://app.localtest.me"
const results = []
const record = (name, ok, evidence) => { results.push({ check: name, result: ok ? "PASS" : "FAIL", evidence }); console.log(ok ? "PASS " : "FAIL ", name, "--", evidence) }
const sleep = ms => new Promise(r => setTimeout(r, ms))

async function signIn(page, email) {
  await page.goto(APP + "/", { waitUntil: "networkidle2" })
  const hops = [page.url().split("?")[0]]
  if (page.url().startsWith("http://localhost:5556/dex/auth")) {
    await Promise.all([page.waitForNavigation({ waitUntil: "networkidle2" }), page.click('a[href^="/dex/auth/local"]')])
    hops.push(page.url().split("?")[0])
    await page.type('input[name="login"]', email)
    await page.type('input[name="password"]', "test")
    await Promise.all([page.waitForNavigation({ waitUntil: "networkidle2" }), page.click('button[type="submit"]')])
  }
  hops.push(page.url())
  return hops
}
const text = page => page.evaluate(() => document.body.innerText)
async function clickButton(page, label) {
  const ok = await page.evaluate(label => {
    const b = [...document.querySelectorAll("button")].find(b => b.innerText.trim() === label && !b.disabled)
    if (b) b.click()
    return !!b
  }, label)
  if (!ok) throw new Error("no button " + label)
}

const browser = await puppeteer.launch({
  executablePath: CHROME,
  headless: true, args: ["--ignore-certificate-errors", "--window-size=1400,1100"], defaultViewport: { width: 1400, height: 1100 },
})
let sid = null
try {
  const page = await browser.newPage()
  page.on("dialog", d => d.accept())
  const hops = await signIn(page, "alice@example.com")
  record("sign-in: app -> Dex -> static user -> UI", page.url().startsWith(APP) && hops[0].includes("localhost:5556/dex"), hops.join(" -> "))
  await page.waitForFunction(() => document.body.innerText.includes("Sessions"), { timeout: 20000 })
  const me = await page.evaluate(() => fetch("/api/me", { headers: { Accept: "application/json" } }).then(async r => ({ status: r.status, body: await r.json() })))
  record("/api/me through Pomerium (assertion reaches the backend, aud accepted)", me.status === 200 && me.body.email === "alice@example.com", JSON.stringify(me))
  const user = await page.evaluate(() => fetch("/.pomerium/user").then(async r => ({ status: r.status, body: await r.text() })))
  console.log("/.pomerium/user", user.status, user.body.slice(0, 300))

  await clickButton(page, "New session")
  await page.waitForSelector('[role="dialog"] input', { visible: true })
  await page.type('[role="dialog"] input', "browser e2e")
  await clickButton(page, "Create")
  await page.waitForFunction(() => /\/sessions\/s-[a-z2-7]{10}$/.test(location.pathname), { timeout: 20000 })
  sid = page.url().split("/").pop()
  const t0 = Date.now()
  await page.waitForFunction(() => document.body.innerText.includes("running"), { timeout: 120000 })
  record("create a session in the UI", true, `${sid} running in the UI after ${((Date.now() - t0) / 1000).toFixed(1)}s`)

  // The screen is drawn on a canvas, over a websocket to the session's host.
  try {
  await page.waitForSelector("canvas", { timeout: 60000 })
  await page.waitForFunction(() => { const c = document.querySelector("canvas"); return c && c.width >= 1000 }, { timeout: 60000 })
  await sleep(4000)
  const canvas = await page.evaluate(() => {
    const c = document.querySelector("canvas"); const ctx = c.getContext("2d")
    const d = ctx.getImageData(0, 0, c.width, c.height).data
    const seen = new Set(); for (let i = 0; i < d.length; i += 4 * 997) seen.add((d[i] << 16) | (d[i + 1] << 8) | d[i + 2])
    return { width: c.width, height: c.height, colours: seen.size }
  })
  record("session page shows the live VNC view", canvas.width === 1280 && canvas.colours > 3, JSON.stringify(canvas) + " body: " + (await text(page)).replace(/\s+/g, " ").slice(0, 200))
  } catch (e) { record("session page shows the live VNC view", false, String(e).slice(0, 200)) }
  await page.screenshot({ path: OUT + "/deploy-local-detail.png" })

  await page.goto(APP + "/", { waitUntil: "networkidle2" })
  await page.waitForFunction(() => document.body.innerText.includes("browser e2e"), { timeout: 20000 })
  await page.screenshot({ path: OUT + "/deploy-local-list.png" })
  record("list page shows the session", true, (await text(page)).replace(/\s+/g, " ").slice(0, 200))

  // What a fetch gets once the Pomerium session is gone: drop the cookies, keep the page.
  const cookies = await browser.cookies()
  console.log("cookies:", cookies.map(c => `${c.name}@${c.domain} httpOnly=${c.httpOnly} secure=${c.secure} sameSite=${c.sameSite}`).join("; "))
  const cdp = await page.createCDPSession()
  await cdp.send("Network.clearBrowserCookies")
  const gone = await page.evaluate(async () => {
    const out = {}
    for (const [label, headers] of [["json", { Accept: "application/json" }], ["any", {}]]) {
      const r = await fetch("/api/me", { headers, redirect: "manual" })
      out[label] = { status: r.status, type: r.type, ctype: r.headers.get("content-type") }
    }
    return out
  })
  record("API fetch without a Pomerium session", gone.json.status === 401 || gone.json.type === "opaqueredirect", JSON.stringify(gone))
  // The UI polls; it should notice and reload into sign-in.
  try {
    await page.waitForFunction(() => location.href.startsWith("http://localhost:5556/dex/"), { timeout: 30000 })
    record("UI copes with a lost session (reloads into sign-in)", true, "page went to " + page.url().split("?")[0])
  } catch (e) {
    record("UI copes with a lost session (reloads into sign-in)", false, "page stayed at " + page.url() + ": " + (await text(page)).replace(/\s+/g, " ").slice(0, 200))
  }

  // Sign in again, then sign out with the UI's link.
  await signIn(page, "alice@example.com")
  await page.waitForFunction(() => document.body.innerText.includes("sign out"), { timeout: 20000 })
  await Promise.all([page.waitForNavigation({ waitUntil: "networkidle2" }), page.evaluate(() => [...document.querySelectorAll("a")].find(a => a.innerText === "sign out").click())])
  const afterSignOut = page.url()
  const body = (await text(page)).replace(/\s+/g, " ").slice(0, 160)
  const again = await page.goto(APP + "/api/me", { waitUntil: "networkidle2" })
  record("sign out", !page.url().startsWith(APP), `sign out link -> ${afterSignOut.split("?")[0]} ("${body}"); then ${APP}/api/me -> ${page.url().split("?")[0]} (${again.status()})`)

  // Someone who can sign in at Dex but is not on Pomerium's list.
  {
    const other = await browser.createBrowserContext()
    const p2 = await other.newPage()
    await signIn(p2, "mallory@example.com")
    const res = await p2.goto(APP + "/api/me", { waitUntil: "networkidle2" })
    const ui = await p2.goto(APP + "/", { waitUntil: "networkidle2" })
    const shown = (await text(p2)).replace(/\s+/g, " ").slice(0, 80)
    record("a signed-in user who is not on the list is refused by Pomerium", res.status() === 403 && ui.status() === 403 && !shown.includes("Sessions"),
      `mallory@example.com: /api/me ${res.status()}, / ${ui.status()} ("${shown}")`)
    await other.close()
  }
  // The routes with their own credentials are not behind the list, or any sign-in.
  {
    process.env.NODE_TLS_REJECT_UNAUTHORIZED = "0" // the local CA
    const host = `https://${sid}.sessions.localtest.me`
    const put = await fetch(host + "/api/artifact-uploads/" + "0".repeat(64), { method: "PUT", body: "x" })
    const vnc = await fetch(host + "/vnc")
    const doc = await fetch(host + "/.well-known/oauth-authorization-server")
    const mcp = await fetch(host + "/mcp", { method: "POST" })
    record("upload, VNC and discovery routes need no sign-in; MCP does", put.status === 404 && !(await put.text()).includes("<html") && vnc.status === 426 && doc.status === 200 && mcp.status === 401,
      `no cookie: PUT upload with an unknown token ${put.status} (the backend's), GET /vnc ${vnc.status}, discovery ${doc.status}, POST /mcp ${mcp.status}`)
  }

  // A real MCP client on the session's host (test/mcp-client.mjs): the owner
  // gets in, someone else on the list gets a token and then the backend's 404.
  {
    const client = email => {
      const run = require("node:child_process").spawnSync(process.execPath, [ROOT + "/test/mcp-client.mjs", `${sid}.sessions.localtest.me`, email], { encoding: "utf8" })
      let out = {}
      try { out = JSON.parse(run.stdout) } catch {}
      return { status: run.status, out }
    }
    const owner = client("alice@example.com")
    record("mcp client: discovery, sign-in, initialize, tools/list, run_js", owner.status === 0,
      `tools ${JSON.stringify(owner.out.tools)}; run_js ${owner.out.run_js}; ${(owner.out.requests ?? []).slice(0, 4).join(", ")}${owner.out.error ? "; error " + owner.out.error : ""}`)
    const stranger = client("bob@example.com")
    record("mcp client: another user is told the session does not exist", stranger.status === 1 && !!stranger.out.token && (stranger.out.requests ?? []).at(-1) === "POST /mcp 404",
      `bob: token ${stranger.out.token ? "issued" : "none"}; ${(stranger.out.requests ?? []).at(-1)}; ${stranger.out.error ?? ""}`.trim())
  }

  // Clean up as alice, unless KEEP=1 (to point test/mcp-oauth.mjs at the session).
  await signIn(page, "alice@example.com")
  if (process.env.KEEP) { console.log("kept", sid); sid = null; throw null }
  const del = await page.evaluate(id => fetch("/api/sessions/" + id, { method: "DELETE" }).then(r => r.status), sid)
  console.log("deleted", sid, del)
  if (del === 204) sid = null
} catch (e) {
  if (e !== null) record("browser flow", false, String(e.stack || e))
} finally {
  await browser.close()
  require("node:fs").writeFileSync(OUT + "/browser-results.json", JSON.stringify(results, null, 2))
  if (sid) console.log("LEFT BEHIND", sid)
}
process.exit(results.some(r => r.result === "FAIL") ? 1 : 0)
